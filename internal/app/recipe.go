package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"labmill-client/internal/account"
	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/store"
)

// The run recipe (owner, 2026-11-11): everything needed to run a job again.
// submit_job saves it as a "recipe" event and as labmill-recipe.json in the
// job's results folder; job_recipe shows it and rerun_job drafts a new job from
// it. It stays on this computer: the account's job record is built from other
// events and never reads it.

const eventRecipe = "recipe" // text is the JSON recipe of one submission

// recipeVersion is the format of recipe.
const recipeVersion = 1

// recipe is one submission, as the user approved it.
type recipe struct {
	Version     int             `json:"version"`
	Job         string          `json:"job"`
	Cluster     string          `json:"cluster"`
	RemoteID    string          `json:"remote_id,omitempty"`
	SubmittedAt time.Time       `json:"submitted_at"`
	Client      string          `json:"client,omitempty"` // Labmill version
	Script      string          `json:"script"`
	Files       []jobFile       `json:"files,omitempty"`
	Spec        backend.JobSpec `json:"spec"`
	Software    []string        `json:"software,omitempty"` // as written, not looked up on the cluster
	Note        string          `json:"note,omitempty"`     // set when rebuilt after an automatic resubmission
}

var (
	moduleLoadRe = regexp.MustCompile(`(?:^|[;&|(]|\bthen\b|\bdo\b)\s*(module\s+(?:load|add)|ml)\s+([A-Za-z0-9][^#;&|)]*)`)
	condaRe      = regexp.MustCompile(`(?:^|[;&|(])\s*(?:conda|mamba|micromamba)\s+(?:activate|run\s+-n)\s+(\S+)`)
	imageRe      = regexp.MustCompile(`(?:^|[;&|(])\s*(?:apptainer|singularity)\s+(?:exec|run|shell)\s+(.*)`)
)

// moduleCmds are the words after "ml" that are commands, not module names.
var moduleCmds = map[string]bool{"purge": true, "list": true, "unload": true, "use": true, "avail": true, "save": true, "restore": true, "reset": true, "swap": true, "show": true, "spider": true}

// imageFlagsWithValue take the next word as their value.
var imageFlagsWithValue = map[string]bool{"--bind": true, "-B": true, "--env": true, "--env-file": true, "--home": true, "-H": true, "--pwd": true, "--workdir": true, "-W": true, "--overlay": true, "-o": true, "--scratch": true, "-S": true, "--mount": true, "--cwd": true, "--security": true, "--apply-cgroups": true, "--network": true, "--hostname": true}

// containerImage is the first word of the arguments of "apptainer exec" that is not a flag or a flag value.
func containerImage(args string) string {
	words := strings.Fields(args)
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case strings.HasPrefix(w, "-"):
			if imageFlagsWithValue[w] {
				i++
			}
		default:
			return w
		}
	}
	return ""
}

// recipeSoftware lists the software a job names: the spec's environment and
// the module, conda and container lines of the script and its files. A module
// without a version is marked, because the cluster's default may change.
func recipeSoftware(spec backend.JobSpec, texts ...string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(kind, name string) {
		item := kind + " " + name
		if kind == "module" && !strings.Contains(name, "/") {
			item += " (no version pinned)"
		}
		if !seen[item] && len(out) < 50 {
			seen[item] = true
			out = append(out, item)
		}
	}
	if e := spec.Environment; e != nil {
		for _, m := range e.Modules {
			add("module", m)
		}
		if e.Venv != nil {
			add("venv from", e.Venv.Requirements)
		}
		if e.Conda != nil {
			add("conda environment from", e.Conda.File)
		}
		if e.Apptainer != nil {
			add("container", e.Apptainer.Image)
		}
	}
	for _, text := range texts {
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if m := moduleLoadRe.FindStringSubmatch(line); m != nil {
				for i, w := range strings.Fields(m[2]) {
					if i == 0 && m[1] == "ml" && moduleCmds[w] {
						break // "ml purge", "ml list": a command, not a module
					}
					if !strings.HasPrefix(w, "-") {
						add("module", w)
					}
				}
			}
			if m := condaRe.FindStringSubmatch(line); m != nil {
				add("conda environment", m[1])
			}
			if m := imageRe.FindStringSubmatch(line); m != nil {
				if img := containerImage(m[1]); img != "" {
					add("container", img)
				}
			}
		}
	}
	return out
}

// buildRecipe is the recipe of j submitted with script and files.
func (t *toolset) buildRecipe(j store.Job, script string, files []jobFile, spec backend.JobSpec) recipe {
	texts := []string{script}
	for _, f := range files {
		texts = append(texts, f.Content)
	}
	return recipe{Version: recipeVersion, Job: j.Name, Cluster: j.Cluster, RemoteID: j.RemoteID, SubmittedAt: t.now().UTC(),
		Client: t.env.ClientVersion, Script: script, Files: files, Spec: spec, Software: recipeSoftware(spec, texts...)}
}

// saveRecipe stores r on the job and next to its results. It never fails the
// submit: the job is already on the cluster. The returned text is a warning
// for the user, or "".
func (t *toolset) saveRecipe(ctx context.Context, j store.Job, r recipe) string {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "I couldn't save the run recipe."
	}
	if err := t.env.Store.AddEvent(ctx, j.ID, eventRecipe, string(b)); err != nil {
		return "I couldn't save the run recipe: " + sanitize(err.Error())
	}
	if j.LocalDir != "" {
		if err := config.WriteFile(filepath.Join(j.LocalDir, "labmill-recipe.json"), append(b, '\n')); err != nil {
			return "The run recipe is saved in Labmill but not in " + sanitize(j.LocalDir) + ": " + sanitize(err.Error())
		}
	}
	return ""
}

// recipeOf is the recipe of job j: the saved one, else (a job submitted
// before recipes) one rebuilt from the job's script, settings and files.
func (t *toolset) recipeOf(ctx context.Context, j store.Job) (recipe, error) {
	stale := false
	if text, ok, err := t.latestEvent(ctx, j.ID, eventRecipe); err != nil {
		return recipe{}, err
	} else if ok {
		var r recipe
		// A resubmission after preemption changes only the remote id. A changed script
		// makes the saved recipe stale: rebuild it from the job.
		if json.Unmarshal([]byte(text), &r) == nil && r.Script != "" && r.Script == j.Script {
			r.RemoteID = j.RemoteID
			return r, nil
		}
		stale = r.Script != ""
	}
	if strings.TrimSpace(j.Script) == "" || j.Cluster == "" {
		return recipe{}, fmt.Errorf("Job %d (%s) has no script yet, so it has no recipe.", j.ID, j.Name)
	}
	var spec backend.JobSpec
	if text, ok, err := t.latestEvent(ctx, j.ID, eventSpec); err != nil {
		return recipe{}, err
	} else if !ok || json.Unmarshal([]byte(text), &spec) != nil {
		return recipe{}, fmt.Errorf("I can't find the settings of job %d, so I can't rebuild its recipe.", j.ID)
	}
	files, err := t.jobFilesOf(ctx, j.ID)
	if err != nil {
		return recipe{}, err
	}
	r := t.buildRecipe(j, j.Script, files, spec)
	r.SubmittedAt = time.Time{} // unknown: the job predates recipes
	if stale {
		r.Note = "The script was changed after the first submission (automatic resubmission). The settings below are those of the first submission; the script is the current one."
	}
	return r, nil
}

// text is the recipe for the user, with the two ways to run it again.
func (r recipe) text(jobID int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Recipe of job %d (%s) on %s", jobID, sanitize(r.Job), sanitize(r.Cluster))
	if !r.SubmittedAt.IsZero() {
		fmt.Fprintf(&b, ", submitted %s", r.SubmittedAt.Local().Format("2006-01-02 15:04"))
	}
	if r.Client != "" {
		fmt.Fprintf(&b, " with Labmill %s", sanitize(r.Client))
	}
	b.WriteString(".\n")
	if r.Note != "" {
		b.WriteString(sanitize(r.Note) + "\n")
	}
	s := r.Spec
	fmt.Fprintf(&b, "Settings: work folder %s", sanitize(s.WorkDir))
	if s.Partition != "" {
		fmt.Fprintf(&b, ", partition %s", sanitize(s.Partition))
	}
	if s.Account != "" {
		fmt.Fprintf(&b, ", account %s", sanitize(s.Account))
	}
	if s.CPUs > 0 {
		fmt.Fprintf(&b, ", %d CPUs", s.CPUs)
	}
	if s.MemoryGB > 0 {
		fmt.Fprintf(&b, ", %d GB memory", s.MemoryGB)
	}
	if s.GPUs > 0 {
		fmt.Fprintf(&b, ", %d GPU %s", s.GPUs, sanitize(s.GPUType))
	}
	if s.Time > 0 {
		fmt.Fprintf(&b, ", time limit %s", s.Time)
	}
	var more []string
	if s.QOS != "" {
		more = append(more, "QOS "+sanitize(s.QOS))
	}
	if s.Nodes > 0 {
		more = append(more, fmt.Sprintf("%d nodes", s.Nodes))
	}
	if len(s.Env) > 0 {
		names := make([]string, 0, len(s.Env))
		for k := range s.Env {
			names = append(names, k)
		}
		sort.Strings(names)
		more = append(more, "environment variables "+sanitize(strings.Join(names, ", ")))
	}
	if len(more) > 0 {
		b.WriteString("; " + strings.Join(more, ", "))
	}
	b.WriteString("\n")
	if len(r.Software) > 0 {
		b.WriteString("Software, as the script names it (versions are not looked up on the cluster):\n")
		for _, x := range r.Software {
			b.WriteString("  - " + sanitize(x) + "\n")
		}
	}
	if len(r.Files) > 0 {
		names := make([]string, len(r.Files))
		for i, f := range r.Files {
			names[i] = f.Name
		}
		fmt.Fprintf(&b, "Extra files: %s\n", sanitize(strings.Join(names, ", ")))
	}
	fmt.Fprintf(&b, "Script:\n%s\n", r.Script)
	fmt.Fprintf(&b, "Run again in Labmill: ask to rerun job %d (the user approves the script again).\n", jobID)
	fmt.Fprintf(&b, "Run again by hand: put the script in a file in %s on %s and run sbatch on it; the same input data must still be there.", sanitize(s.WorkDir), sanitize(r.Cluster))
	return b.String()
}

func (t *toolset) now() time.Time {
	if t.env.Now != nil {
		return t.env.Now()
	}
	return time.Now()
}

type recipeInput struct {
	JobID int64  `json:"job_id"`
	Name  string `json:"name"`
}

func (t *toolset) jobRecipe() agent.Tool {
	return agent.Tool{
		Name: "job_recipe",
		Description: "Show how a submitted job was run: its exact script, settings, extra files and the software it names, " +
			"and how to run it again. It reads Labmill's saved record and changes nothing. Use it when the user asks how a job was run or wants to reproduce it.",
		Schema: objSchema(map[string]any{"job_id": intProp("job id")}, "job_id"),
		Run: func(ctx context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			var in recipeInput
			if err := decode(input, &in); err != nil {
				return "", err
			}
			j, err := t.job(ctx, in.JobID)
			if err != nil {
				return "", err
			}
			r, err := t.recipeOf(ctx, j)
			if err != nil {
				return "", err
			}
			return r.text(j.ID), nil
		},
	}
}

func (t *toolset) rerunJob() agent.Tool {
	return agent.Tool{
		Name: "rerun_job",
		Description: "Draft a new job that runs a submitted job's recipe again: the same cluster, script, settings and extra files, in the same work folder. " +
			"It submits nothing: it returns the new job's id, then call submit_job so the user can review and approve the script. " +
			"Tell the user that the new job writes to the same work folder, so its output can replace the old output there. " +
			"Use it when the user says to run, repeat or reproduce a job. Change the script or settings with draft_job instead when the user wants something different.",
		Schema: objSchema(map[string]any{
			"job_id": intProp("id of the job to run again"),
			"name":   strProp("name of the new job (default: the old name and -rerun)"),
		}, "job_id"),
		Run: func(ctx context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			var in recipeInput
			if err := decode(input, &in); err != nil {
				return "", err
			}
			j, err := t.job(ctx, in.JobID)
			if err != nil {
				return "", err
			}
			if j.RemoteID == "" {
				return "", fmt.Errorf("Job %d was never submitted. Use submit_job for it.", j.ID)
			}
			if j.State == store.StateSubmitted || j.State == store.StatePending || j.State == store.StateRunning {
				return "", fmt.Errorf("Job %d may still be running (state %s). Run job_status first; a second run in the same work folder can damage the output of the first.", j.ID, j.State)
			}
			r, err := t.recipeOf(ctx, j)
			if err != nil {
				return "", err
			}
			c, err := t.cluster(r.Cluster)
			if err != nil {
				return "", err
			}
			taken := map[string]bool{}
			all, aerr := t.env.Store.Jobs(ctx, store.JobFilter{})
			if aerr != nil {
				return "", fmt.Errorf("I couldn't check the other jobs: %w", aerr)
			}
			for _, o := range all {
				taken[o.Name] = true
				if o.ID != j.ID && o.Cluster == c.Name && activeState(o.State) && (sameFolder(o, j.RemoteDir) || sameFolder(o, r.Spec.WorkDir) || t.specWorkDirIn(ctx, o.ID, r.Spec.WorkDir)) {
					return "", fmt.Errorf("Job %d (%s) may still be running in the same work folder (state %s). Run job_status first; a second run there can damage its output.", o.ID, sanitize(o.Name), o.State)
				}
			}
			name := in.Name
			if name == "" {
				name = safeJobName(j.Name + "-rerun")
			}
			if name != safeJobName(name) {
				return "", fmt.Errorf("Job name %q isn't allowed: use letters, digits, dots, dashes and underscores.", sanitize(name))
			}
			base := name
			if len(base) > 60 {
				base = base[:60]
			}
			for n := 2; taken[name]; n++ {
				name = fmt.Sprintf("%s%d", base, n)
			}
			if len(r.Files) > 0 {
				if err := t.checkJobFilesTarget(c, r.Spec); err != nil {
					return "", err
				}
			}
			// The new job is as private as the old one. Read that before CreateJob, so a
			// failure leaves no draft without the mark.
			evs, err := t.env.Store.Events(ctx, j.ID, 0)
			if err != nil {
				return "", fmt.Errorf("I couldn't read the old job's settings: %w", err)
			}
			private := account.Private(evs)
			id, err := t.env.Store.CreateJob(ctx, store.Job{
				Name: name, Cluster: c.Name, Backend: j.Backend, State: store.StateDraft, Script: r.Script,
				LocalDir: filepath.Join(t.env.ResultsDir, name), RemoteDir: j.RemoteDir,
			})
			if err != nil {
				return "", fmt.Errorf("I couldn't create the job: %w", err)
			}
			specJSON, err := json.Marshal(r.Spec)
			if err == nil {
				err = t.env.Store.AddEvent(ctx, id, eventSpec, string(specJSON))
			}
			if err == nil {
				err = t.saveJobFiles(ctx, id, r.Files)
			}
			if err == nil {
				if private {
					err = t.env.Store.AddEvent(ctx, id, account.EventPrivate, "")
				}
			}
			if err != nil {
				return "", fmt.Errorf("I couldn't save the new job: %w", err)
			}
			return fmt.Sprintf("Drafted job %d (%s) on %s from the recipe of job %d. It is not submitted. Work folder: %s (the same as job %d). Call submit_job %d so the user can review the script.\n%s",
				id, name, c.Name, j.ID, sanitize(r.Spec.WorkDir), j.ID, id, r.Script), nil
		},
	}
}

// activeState reports a job that may still run on the cluster.
func activeState(s store.JobState) bool {
	return s == store.StateSubmitted || s == store.StatePending || s == store.StateRunning
}

// sameFolder reports whether job o works in dir or below it: by its remote
// folder or by a path in its script.
func sameFolder(o store.Job, dir string) bool {
	dir = path.Clean(dir)
	if dir == "." || dir == "/" {
		return false
	}
	return (o.RemoteDir != "" && inFolder(dir, o.RemoteDir)) || mentionsFolder(o.Script, dir)
}

// specWorkDirIn reports whether job id has dir (or a folder below it) as the
// work folder in its saved settings.
func (t *toolset) specWorkDirIn(ctx context.Context, id int64, dir string) bool {
	dir = path.Clean(dir)
	if dir == "." || dir == "/" {
		return false
	}
	var spec backend.JobSpec
	text, ok, err := t.latestEvent(ctx, id, eventSpec)
	return err == nil && ok && json.Unmarshal([]byte(text), &spec) == nil && spec.WorkDir != "" && inFolder(dir, spec.WorkDir)
}
