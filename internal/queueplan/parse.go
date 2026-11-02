package queueplan

import (
	"strconv"
	"strings"
	"time"
)

// ParseMaxWall reads a Slurm time: "UNLIMITED" or "INFINITE" (0, no limit), "mm", "mm:ss", "hh:mm:ss", "d-hh",
// "d-hh:mm" or "d-hh:mm:ss". ok is false for empty text, "NONE", "n/a" and anything else that is no time.
func ParseMaxWall(s string) (time.Duration, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "UNLIMITED" || s == "INFINITE" {
		return 0, true
	}
	days := 0
	if d, rest, found := strings.Cut(s, "-"); found {
		n, err := strconv.Atoi(d)
		if err != nil || n < 0 {
			return 0, false
		}
		days, s = n, rest
		// With days the first field is hours: "1-12" is 36 h.
		parts := strings.Split(s, ":")
		if len(parts) > 3 {
			return 0, false
		}
		var v [3]int
		for i, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 {
				return 0, false
			}
			v[i] = n
		}
		return time.Duration(days)*24*time.Hour + time.Duration(v[0])*time.Hour + time.Duration(v[1])*time.Minute + time.Duration(v[2])*time.Second, true
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 || s == "" {
		return 0, false
	}
	var v []int
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, false
		}
		v = append(v, n)
	}
	switch len(v) {
	case 1: // minutes
		return time.Duration(v[0]) * time.Minute, true
	case 2: // minutes:seconds
		return time.Duration(v[0])*time.Minute + time.Duration(v[1])*time.Second, true
	}
	return time.Duration(v[0])*time.Hour + time.Duration(v[1])*time.Minute + time.Duration(v[2])*time.Second, true
}

// QOS is one row of `sacctmgr show qos -nP format=Name,MaxWall,Preempt,PreemptMode,UsageFactor`.
type QOS struct {
	Name        string
	MaxWall     time.Duration // 0: no limit or not set
	Preempt     []string      // QOS names this one may preempt
	PreemptMode string        // lower case: cluster, off, cancel, requeue, suspend
	UsageFactor float64       // 1 when not set
}

// ParseQOS reads the output of `sacctmgr show qos -nP format=Name,MaxWall,Preempt,PreemptMode,UsageFactor` (pipe
// separated). Short or empty lines are skipped, so hidden or empty output gives no QOS.
func ParseQOS(out string) []QOS {
	var qs []QOS
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) < 5 || f[0] == "" || f[0] == "Name" {
			continue
		}
		q := QOS{Name: f[0], PreemptMode: strings.ToLower(f[3]), UsageFactor: 1}
		q.MaxWall, _ = ParseMaxWall(f[1])
		for _, n := range strings.Split(f[2], ",") {
			if n = strings.TrimSpace(n); n != "" {
				q.Preempt = append(q.Preempt, n)
			}
		}
		if u, err := strconv.ParseFloat(f[4], 64); err == nil && u >= 0 {
			q.UsageFactor = u
		}
		qs = append(qs, q)
	}
	return qs
}

// ParseAssocQOS reads `sacctmgr show assoc user=$USER -nP format=QOS,DefaultQOS`: the QOS the user may use and the
// default one. readable is false when the output has no row, which is what a hidden or denied sacctmgr gives; the
// caller must then leave Allowed unset (low confidence).
func ParseAssocQOS(out string) (allowed []string, def string, readable bool) {
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) < 2 || f[0] == "QOS" || (f[0] == "" && f[1] == "") {
			continue
		}
		readable = true
		for _, n := range strings.Split(f[0], ",") {
			if n = strings.TrimSpace(n); n != "" && !seen[n] {
				seen[n] = true
				allowed = append(allowed, n)
			}
		}
		if def == "" {
			def = f[1]
		}
	}
	return allowed, def, readable
}

// Partition is one line of `scontrol show partition -o`.
type Partition struct {
	Name         string
	Default      bool
	MaxWall      time.Duration // 0: no limit
	PriorityTier int
	PreemptMode  string   // lower case, "" when not shown
	AllowQOS     []string // empty: ALL
	Hidden       bool
}

// ParsePartitions reads `scontrol show partition -o` (one partition per line, Key=Value fields).
func ParsePartitions(out string) []Partition {
	var ps []Partition
	for _, line := range strings.Split(out, "\n") {
		kv := map[string]string{}
		for _, w := range strings.Fields(line) {
			if k, v, ok := strings.Cut(w, "="); ok {
				kv[k] = v
			}
		}
		if kv["PartitionName"] == "" {
			continue
		}
		p := Partition{Name: kv["PartitionName"], Default: kv["Default"] == "YES", Hidden: kv["Hidden"] == "YES",
			PreemptMode: strings.ToLower(kv["PreemptMode"])}
		p.MaxWall, _ = ParseMaxWall(kv["MaxTime"])
		p.PriorityTier, _ = strconv.Atoi(kv["PriorityTier"])
		if a := kv["AllowQos"]; a != "" && a != "ALL" {
			p.AllowQOS = strings.Split(a, ",")
		}
		ps = append(ps, p)
	}
	return ps
}

// Assoc is the parsed `sacctmgr show assoc` result. Readable false means the output was hidden or denied.
type Assoc struct {
	Allowed  []string
	Default  string
	Readable bool
}

func preemptive(mode string) bool { return mode == "requeue" || mode == "cancel" || mode == "suspend" }

// BuildProbe turns parsed probe output into a Probe. Rules (docs/preemptible-qos.md):
//   - Q1 is the default partition. Without one, the first visible partition stands in.
//   - A partition that preempts (requeue or cancel mode) with a lower priority tier than Q1 is Q3 and is assumed to
//     start sooner (it borrows idle nodes); any other visible partition is Q2.
//   - A QOS is a candidate only for a readable sacctmgr and only when the user's association lists it: Allowed is
//     never set from hidden output. A QOS that others preempt, or with a preempting mode, is Q3; one with a usage
//     factor above 1 or that preempts others is Q6; the user's own default QOS, if not "normal", is Q5.
func BuildProbe(parts []Partition, qos []QOS, as Assoc) Probe {
	pr := Probe{Hidden: !as.Readable}
	var def *Partition
	for i := range parts {
		if parts[i].Default {
			def = &parts[i]
			break
		}
	}
	if def == nil {
		for i := range parts {
			if !parts[i].Hidden {
				def = &parts[i]
				break
			}
		}
	}
	if def != nil {
		pr.Default = Candidate{Name: def.Name, MaxWall: def.MaxWall, Allowed: true}
	}
	for _, p := range parts {
		if p.Hidden || (def != nil && p.Name == def.Name) {
			continue
		}
		c := Candidate{Name: p.Name, MaxWall: p.MaxWall, Allowed: true, Technique: Q2, UsageFactor: 1}
		if preemptive(p.PreemptMode) && def != nil && p.PriorityTier < def.PriorityTier {
			c.Technique, c.StartsSoon, c.CancelMode = Q3, true, p.PreemptMode == "cancel"
		}
		pr.Candidates = append(pr.Candidates, c)
	}
	preempted := map[string]bool{}
	for _, q := range qos {
		for _, n := range q.Preempt {
			preempted[n] = true
		}
	}
	allowed := map[string]bool{}
	for _, n := range as.Allowed {
		allowed[n] = true
	}
	for _, q := range qos {
		if q.Name == "normal" && q.Name != as.Default {
			continue
		}
		c := Candidate{Name: q.Name, IsQOS: true, MaxWall: q.MaxWall, UsageFactor: q.UsageFactor,
			Allowed: as.Readable && allowed[q.Name], CancelMode: q.PreemptMode == "cancel"}
		switch {
		case preempted[q.Name] || preemptive(q.PreemptMode):
			c.Technique, c.StartsSoon = Q3, true
		case q.UsageFactor > 1 || len(q.Preempt) > 0:
			c.Technique = Q6
		case q.Name == as.Default && q.Name != "normal":
			c.Technique, c.IsDefault = Q5, true
		default:
			continue
		}
		pr.Candidates = append(pr.Candidates, c)
	}
	return pr
}
