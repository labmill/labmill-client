package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/rules"
	"labmill-client/internal/store"
)

// M61: the global rules for Slurm clusters in draft_job. The cluster's facts
// come from the site profile (Cluster.yaml preemptible:, local_scratch:,
// global_scratch:; SiteInfo.SlurmFor), the rules from the slurm-user skill
// (references/placement-and-preemption.md).

// eventPlan is the job's placement plan (JSON of placementPlan), recorded by
// draft_job next to the "spec" event; the latest one wins. submit_job shows
// it in the review, and the job monitor (M61.3) reads the fallback from it.
const eventPlan = "plan"

// Where a Slurm job keeps its working data (draft_job's scratch).
const (
	scratchLocal     = "local"      // node-local scratch of the compute node (single-node jobs)
	scratchGlobal    = "global"     // the cluster's shared scratch
	scratchJobFolder = "job_folder" // the job's own folder, where its data was uploaded (default)
)

// maxResubmits is how many times Labmill resubmits a preemptible job that was
// preempted or lost its node, before the one submit on the fallback partition.
const maxResubmits = 2

// placementPlan is what the review shows about a Slurm job's placement (M61).
type placementPlan struct {
	Scratch     string `json:"scratch"`      // scratchLocal | scratchGlobal | scratchJobFolder
	ScratchPath string `json:"scratch_path"` // the node-local or global folder; "" for the job folder
	ScratchWhy  string `json:"scratch_why"`
	Preemptible bool   `json:"preemptible"`
	Partition   string `json:"partition"` // as submitted; "" is the cluster default
	QOS         string `json:"qos,omitempty"`
	Requeue     bool   `json:"requeue,omitempty"`
	// FallbackPartition is the non-preemptible partition for the submit after
	// the failed preemptible attempts; "" is the cluster's default partition.
	// Only meaningful when Preemptible.
	FallbackPartition string `json:"fallback_partition,omitempty"`
	// FallbackNeedsPartition: no fallback partition is known and the cluster
	// has no default one, so the last step needs a partition the user names.
	FallbackNeedsPartition bool   `json:"fallback_needs_partition,omitempty"`
	MaxResubmits           int    `json:"max_resubmits,omitempty"`
	PartitionWhy           string `json:"partition_why"`
	// Estimate is the scheduler's expected start (sbatch --test-only); ""
	// when it could not be estimated.
	Estimate string `json:"estimate,omitempty"`
}

// review is the plan in plain words for the review and for draft_job's
// answer: the storage choice and why, the partition, the fallback plan (so
// the approval covers the resubmissions) and the start estimate.
func (p placementPlan) lines() []string {
	var lines []string
	switch p.Scratch {
	case scratchLocal:
		lines = append(lines, fmt.Sprintf("Storage: node-local scratch %s (single-node job). The job copies its folder there, runs on the fast local disk, copies the results back to its folder and removes its local folder only after a good copy. %s", p.ScratchPath, p.ScratchWhy))
	case scratchGlobal:
		lines = append(lines, fmt.Sprintf("Storage: the job runs in its folder on the shared scratch %s; nothing is removed by the job. %s", p.ScratchPath, p.ScratchWhy))
	default:
		lines = append(lines, "Storage: the job runs in its own folder on shared storage. "+p.ScratchWhy)
	}
	part := p.Partition
	if part == "" {
		part = "the cluster's default partition"
	}
	if p.Preemptible {
		s := "Partition: " + part + " (preemptible"
		if p.QOS != "" {
			s += ", qos " + p.QOS
		}
		if p.Requeue {
			s += ", --requeue"
		}
		s += "). " + p.PartitionWhy
		fb := p.FallbackPartition
		switch {
		case fb == "" && p.FallbackNeedsPartition:
			fb = "a non-preemptible partition (this cluster has no default partition and none was named: tell me which one to use, and I redraft with spec.partition)"
		case fb == "":
			fb = "the cluster's default partition"
		}
		s += fmt.Sprintf(" If it is preempted or a node fails, Labmill resubmits it up to %d more times, then once on %s; your approval of this job covers those resubmissions.", p.MaxResubmits, fb)
		lines = append(lines, s)
	} else {
		lines = append(lines, "Partition: "+part+" (not preemptible). "+p.PartitionWhy)
	}
	if p.Estimate != "" {
		lines = append(lines, "Estimated start: "+p.Estimate+" (the scheduler's estimate, it can change).")
	} else {
		lines = append(lines, "Estimated start: not available.")
	}
	for i, l := range lines {
		lines[i] = sanitize(strings.TrimSpace(l))
	}
	return lines
}

// answer is the plan, one sentence group per line, for draft_job's answer.
func (p placementPlan) answer() string { return strings.Join(p.lines(), "\n") }

// summary is the plan on one line (no trailing full stop) for the approval
// summary, whose parts are joined with ". ".
func (p placementPlan) summary() string {
	return strings.TrimSuffix(strings.Join(p.lines(), " "), ".")
}

// slurmAsk is draft_job's placement inputs.
type slurmAsk struct {
	Scratch     string
	Preemptible *bool
}

// planSlurm applies the M61 rules to spec of a Slurm job (spec.WorkDir is
// set): it validates the model's scratch/preemptible choice against the
// cluster's profile, sets spec's LocalScratch, Partition, QOS and Requeue,
// and returns the plan. Errors are plain messages for the model.
func (t *toolset) planSlurm(ctx context.Context, c config.Cluster, be backend.Backend, j store.Job, spec *backend.JobSpec, ask slurmAsk) (placementPlan, error) {
	h := t.env.Site.SlurmFor(c.Name)
	plan := placementPlan{Scratch: scratchJobFolder}
	spec.MailOnEnd = t.env.MailOnEnd != nil && t.env.MailOnEnd() // M64.3
	if spec.Environment != nil {
		spec.Environment.SoftwareDir = h.SoftwareDir // from the profile, never from the model (M67.1)
	}

	switch ask.Scratch {
	case "", scratchJobFolder:
		if h.LocalScratch != nil || h.GlobalScratch != nil {
			plan.ScratchWhy = "Why: no scratch was chosen for this job."
			if ask.Scratch == scratchJobFolder {
				plan.ScratchWhy = "Why: the job folder was chosen."
			}
		}
	case scratchLocal:
		l := h.LocalScratch
		switch {
		case l == nil:
			return plan, fmt.Errorf("No node-local scratch is known for %s, so scratch local isn't possible. Use scratch global (if cluster_info names a shared scratch) or job_folder.", c.Name)
		case spec.Nodes > 1:
			return plan, fmt.Errorf("Node-local scratch only works for a single-node job, and this one asks for %d nodes. Use scratch global or job_folder.", spec.Nodes)
		}
		if l.SizeGB > 0 {
			if total := t.manifestBytes(ctx, j.ID); total > int64(l.SizeGB)*1000*1000*1000*8/10 {
				return plan, fmt.Errorf("The job's input (%s) doesn't fit comfortably in the node-local scratch (%d GB per node). Use scratch global or job_folder.", humanBytes(total), l.SizeGB)
			}
		}
		spec.LocalScratch = l
		plan.Scratch = scratchLocal
		plan.ScratchPath = localScratchLabel(*l)
		plan.ScratchWhy = "Why: node-local disk is the fastest and cleans itself up."
		if l.SizeGB == 0 {
			plan.ScratchWhy += " Its size is not known, so the job fails at once if the folder is too big."
		}
	case scratchGlobal:
		g := h.GlobalScratch
		if g == nil {
			return plan, fmt.Errorf("No shared scratch is known for %s, so scratch global isn't possible. Use job_folder.", c.Name)
		}
		if !g.Covers(path.Clean(spec.WorkDir)) {
			if j.RemoteDir != "" && j.ID != 0 && spec.WorkDir == j.RemoteDir {
				return plan, fmt.Errorf("The data of job %d is in %s, not under the shared scratch %s. Upload it again with upload_data (remote_dir under %s, for example %s/labmill/%s), or use scratch job_folder.", j.ID, sanitize(j.RemoteDir), g.Path, g.Path, g.Path, j.Name)
			}
			return plan, fmt.Errorf("work_dir %s is not under the shared scratch %s. Give a work_dir below it, or use scratch job_folder.", sanitize(spec.WorkDir), g.Path)
		}
		plan.Scratch = scratchGlobal
		plan.ScratchPath = g.Path
		plan.ScratchWhy = "Why: the shared scratch is mounted on the compute nodes and is meant for working data."
		if g.PurgeDays > 0 {
			plan.ScratchWhy += fmt.Sprintf(" Files there are purged after %d days, so download the results soon.", g.PurgeDays)
		}
	default:
		return plan, fmt.Errorf("scratch must be %q, %q or %q (got %q).", scratchLocal, scratchGlobal, scratchJobFolder, sanitize(ask.Scratch))
	}

	// Preemptible first, when the profile has one and the job fits. The
	// partition the model named becomes the fallback after the failed
	// preemptible attempts.
	p := h.Preemptible
	var info backend.ClusterInfo
	if p != nil {
		info, _ = be.Info(ctx) // best effort: limits and the default partition
	}
	switch {
	case ask.Preemptible != nil && *ask.Preemptible && p == nil:
		return plan, fmt.Errorf("No preemptible resource is known for %s, so preemptible true isn't possible. Leave preemptible out.", c.Name)
	case ask.Preemptible != nil && !*ask.Preemptible:
		plan.PartitionWhy = "Why: you asked for a normal (non-preemptible) partition."
	case p == nil:
	case exceedsPartition(info, p.Partition, spec.Time):
		if ask.Preemptible != nil {
			return plan, fmt.Errorf("The time limit is longer than the preemptible partition %s allows. Shorten the job, or leave preemptible out.", p.Partition)
		}
		plan.PartitionWhy = "Why: the time limit is longer than the preemptible partition allows."
	default:
		own := spec.Partition
		fallback := p.FallbackPartition
		switch {
		case p.Partition != "":
			if own != "" && own != p.Partition {
				fallback = own
			}
			spec.Partition = p.Partition
		case own != "":
			fallback = own // a QOS-only resource: the same partition without the QOS
		}
		spec.QOS, spec.Requeue = p.QOS, p.Requeue
		plan.Preemptible = true
		plan.QOS, plan.Requeue = p.QOS, p.Requeue
		plan.FallbackPartition = fallback
		plan.FallbackNeedsPartition = fallback == "" && !hasDefaultPartition(info)
		plan.MaxResubmits = maxResubmits
		plan.PartitionWhy = "Why: the preemptible partition is tried first; it is free when idle nodes exist."
	}
	plan.Partition = spec.Partition
	return plan, nil
}

// exceedsPartition reports whether limit is longer than the time limit of
// partition (from sinfo; false when unknown or unlimited).
func exceedsPartition(info backend.ClusterInfo, partition string, limit time.Duration) bool {
	if partition == "" || limit <= 0 {
		return false
	}
	for _, p := range info.Partitions {
		if p.Name == partition && p.MaxTime > 0 && limit > p.MaxTime {
			return true
		}
	}
	return false
}

// hasDefaultPartition reports whether the cluster has a default partition;
// true when sinfo told nothing (unknown is not "none").
func hasDefaultPartition(info backend.ClusterInfo) bool {
	if len(info.Partitions) == 0 {
		return true
	}
	for _, p := range info.Partitions {
		if p.Default {
			return true
		}
	}
	return false
}

// manifestBytes is the staged size of a job's input (0 when unknown).
func (t *toolset) manifestBytes(ctx context.Context, jobID int64) int64 {
	if jobID == 0 {
		return 0
	}
	entries, err := t.env.Store.Manifest(ctx, jobID)
	if err != nil {
		return 0
	}
	var n int64
	for _, e := range entries {
		n += e.Size
	}
	return n
}

// humanBytes is n in decimal gigabytes with one decimal ("12.5 GB").
func humanBytes(n int64) string { return fmt.Sprintf("%.1f GB", float64(n)/1e9) }

// localScratchLabel shows where the node-local folder is.
func localScratchLabel(l rules.LocalScratch) string {
	switch {
	case l.EnvVar != "" && l.Path != "":
		return "$" + l.EnvVar + " (else " + l.Path + ")"
	case l.EnvVar != "":
		return "$" + l.EnvVar
	}
	return l.Path
}

// estimateStart asks the scheduler for the expected start of script
// (sbatch --test-only; nothing is submitted). "" when it can't tell.
func estimateStart(ctx context.Context, be backend.Backend, script string) string {
	e, ok := be.(backend.StartEstimator)
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	est, err := e.EstimateStart(ctx, script)
	if err != nil || est.Start == "" {
		return ""
	}
	s := est.Start + " (cluster time)"
	if est.Partition != "" {
		s += ", partition " + est.Partition
	}
	if est.Nodes != "" {
		s += ", nodes " + est.Nodes
	}
	return s
}

// jobPlan reads the job's latest placement plan; ok is false for a job
// without one (drafted before M61).
func (t *toolset) jobPlan(ctx context.Context, jobID int64) (placementPlan, bool) {
	text, ok, err := t.latestEvent(ctx, jobID, eventPlan)
	var p placementPlan
	if err != nil || !ok || json.Unmarshal([]byte(text), &p) != nil {
		return placementPlan{}, false
	}
	return p, true
}

// planReview is the review text of the job's plan, "" without one.
func (t *toolset) planReview(ctx context.Context, jobID int64) string {
	if p, ok := t.jobPlan(ctx, jobID); ok {
		return p.summary()
	}
	return ""
}

// slurmPlacementNote states the cluster's preemptible and scratch facts in
// plain words for cluster_info, and how draft_job uses them.
func slurmPlacementNote(h rules.SlurmHints) string {
	if h.Zero() {
		return ""
	}
	var b strings.Builder
	b.WriteString("Placement rules (draft_job: scratch, preemptible):\n")
	if p := h.Preemptible; p != nil {
		var r []string
		if p.Partition != "" {
			r = append(r, "partition "+p.Partition)
		}
		if p.QOS != "" {
			r = append(r, "qos "+p.QOS)
		}
		fmt.Fprintf(&b, "- Preemptible: %s", strings.Join(r, ", "))
		if p.Requeue {
			b.WriteString("; submitted with --requeue")
		}
		fb := p.FallbackPartition
		if fb == "" {
			fb = "the default partition"
		}
		fmt.Fprintf(&b, ". Tried first (a preempted job is resubmitted up to %d more times, then once on %s). Leave preemptible out to use it; preemptible false for a job that must not be interrupted or is longer than the partition allows.\n", maxResubmits, fb)
	} else {
		b.WriteString("- Preemptible: no preemptible partition is known; use the normal partitions.\n")
	}
	if l := h.LocalScratch; l != nil {
		fmt.Fprintf(&b, "- Node-local scratch: %s", localScratchLabel(*l))
		if l.SizeGB > 0 {
			fmt.Fprintf(&b, ", about %d GB per node", l.SizeGB)
		} else {
			b.WriteString(", size not known")
		}
		b.WriteString(". scratch local (single-node jobs only) stages in, runs there, stages out and removes the local folder after a good copy.\n")
	}
	if g := h.GlobalScratch; g != nil {
		fmt.Fprintf(&b, "- Shared scratch: %s", g.Path)
		if g.PurgeDays > 0 {
			fmt.Fprintf(&b, ", files purged after %d days", g.PurgeDays)
		}
		b.WriteString(". scratch global runs the job in your own folder below it (for example <that path>/<your login>/labmill/<job>); upload_data to a remote_dir there first, and give work_dir for a job without data.\n")
	}
	if h.SoftwareDir != "" {
		fmt.Fprintf(&b, "- Software folder: %s. draft_job environment (venv, conda, apptainer) builds reusable environments there, once per requirements hash; they are never cleaned up.\n", h.SoftwareDir)
	}
	b.WriteString("- Order: node-local scratch for a single-node job, else the shared scratch, else the job's own folder. Ask for the smallest node that fits (cores, memory, GPU type).\n")
	return b.String()
}
