// Package testcase loads and validates the test corpus: small research tasks
// under tests/cases/<id>/ (case.yaml, gen.sh, check.sh) that the
// labmill-testrun runner plays against a real cluster. It does no I/O beyond
// reading the case folders.
package testcase

import (
	"bytes"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Failure classes a failing case may expect.
var classes = map[string]bool{
	"timeout": true, "out_of_memory": true, "exit_nonzero": true,
	"submit_rejected": true, "missing_software": true, "data_missing": true,
}

var agentShould = map[string]bool{"explain": true, "fix_and_resubmit": true, "ask_user": true}

var idRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Defaults and limits.
const (
	DefaultTimeBudget = 300
	MinTimeBudget     = 10
	MaxTimeBudget     = 900
	DefaultDataMax    = 5000000
)

// Needs lists what the cluster must offer. A case is skipped when it does not.
type Needs struct {
	CPUs    int      `yaml:"cpus"`
	MemMB   int      `yaml:"mem_mb"`
	GPU     bool     `yaml:"gpu"`
	MPI     bool     `yaml:"mpi"`
	Modules []string `yaml:"modules"`
}

// Expect is the outcome the runner compares with.
type Expect struct {
	Outcome       string   `yaml:"outcome"`
	FailureClass  string   `yaml:"failure_class"`
	AgentShould   string   `yaml:"agent_should"`
	AgentKeywords []string `yaml:"agent_keywords"`
}

// Case is one test case (case.yaml) plus its folder.
type Case struct {
	ID           string   `yaml:"id"`
	Title        string   `yaml:"title"`
	Tags         []string `yaml:"tags"`
	Prompt       string   `yaml:"prompt"`
	Needs        Needs    `yaml:"needs"`
	TimeBudget   int      `yaml:"time_budget"`
	DataBytesMax int      `yaml:"data_bytes_max"`
	Expect       Expect   `yaml:"expect"`

	Dir string `yaml:"-"` // folder inside the FS, for example "py-numpy-sum"
}

// Group is the first tag.
func (c Case) Group() string {
	if len(c.Tags) == 0 {
		return ""
	}
	return c.Tags[0]
}

// Parse reads and checks one case.yaml. dir is the folder name; the id must equal it.
func Parse(dir string, data []byte) (Case, error) {
	var c Case
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("case.yaml: %w", err)
	}
	c.Dir = dir
	if !bytes.Contains(data, []byte("time_budget")) {
		c.TimeBudget = DefaultTimeBudget
	}
	if c.DataBytesMax == 0 {
		c.DataBytesMax = DefaultDataMax
	}
	return c, c.check()
}

func (c Case) check() error {
	switch {
	case !idRE.MatchString(c.ID):
		return fmt.Errorf("id %q: use lowercase letters, digits and dashes", c.ID)
	case c.ID != c.Dir:
		return fmt.Errorf("id %q differs from folder %q", c.ID, c.Dir)
	case strings.TrimSpace(c.Title) == "":
		return fmt.Errorf("title is empty")
	case len(c.Tags) == 0:
		return fmt.Errorf("tags is empty")
	case strings.TrimSpace(c.Prompt) == "":
		return fmt.Errorf("prompt is empty")
	case c.TimeBudget < MinTimeBudget || c.TimeBudget > MaxTimeBudget:
		return fmt.Errorf("time_budget %d: use %d to %d", c.TimeBudget, MinTimeBudget, MaxTimeBudget)
	case c.DataBytesMax < 0 || c.DataBytesMax > DefaultDataMax:
		return fmt.Errorf("data_bytes_max %d: use 0 (default) to %d", c.DataBytesMax, DefaultDataMax)
	}
	for _, t := range c.Tags {
		if !idRE.MatchString(t) {
			return fmt.Errorf("tag %q: use lowercase letters, digits and dashes", t)
		}
	}
	e := c.Expect
	switch e.Outcome {
	case "completed":
		if e.FailureClass != "" || e.AgentShould != "" || len(e.AgentKeywords) > 0 {
			return fmt.Errorf("expect: failure_class, agent_should and agent_keywords belong to outcome failed")
		}
	case "failed":
		if !classes[e.FailureClass] {
			return fmt.Errorf("expect.failure_class %q: not a known class", e.FailureClass)
		}
		if !agentShould[e.AgentShould] {
			return fmt.Errorf("expect.agent_should %q: use explain, fix_and_resubmit or ask_user", e.AgentShould)
		}
		if e.AgentShould == "explain" && len(e.AgentKeywords) == 0 {
			return fmt.Errorf("expect.agent_keywords: explain needs at least one word")
		}
	default:
		return fmt.Errorf("expect.outcome %q: use completed or failed", e.Outcome)
	}
	return nil
}

// Load reads every folder of fsys that holds a case. root is the folder with
// the cases ("." for tests/cases). All problems are returned together; the
// cases that are valid are returned too. The result is sorted by id.
func Load(fsys fs.FS, root string) ([]Case, []error) {
	ents, err := fs.ReadDir(fsys, root)
	if err != nil {
		return nil, []error{err}
	}
	var cases []Case
	var errs []error
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		fail := func(err error) { errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err)) }
		dir := path.Join(root, e.Name())
		data, err := fs.ReadFile(fsys, path.Join(dir, "case.yaml"))
		if err != nil {
			fail(err)
			continue
		}
		c, err := Parse(e.Name(), data)
		if err != nil {
			fail(err)
			continue
		}
		gen, err := fs.ReadFile(fsys, path.Join(dir, "gen.sh"))
		if err != nil || len(bytes.TrimSpace(gen)) == 0 {
			fail(fmt.Errorf("gen.sh is missing or empty"))
			continue
		}
		chk, err := fs.ReadFile(fsys, path.Join(dir, "check.sh"))
		if err != nil {
			fail(fmt.Errorf("check.sh is missing"))
			continue
		}
		if len(bytes.TrimSpace(chk)) == 0 && c.Expect.Outcome == "completed" {
			fail(fmt.Errorf("check.sh is empty: only a failed case may have an empty check"))
			continue
		}
		cases = append(cases, c)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })
	return cases, errs
}

// Filter keeps the cases whose id matches glob (path.Match; empty = all) and
// that carry every tag in tags.
func Filter(cases []Case, glob string, tags []string) ([]Case, error) {
	if glob != "" {
		if _, err := path.Match(glob, ""); err != nil {
			return nil, fmt.Errorf("-cases %q: %w", glob, err)
		}
	}
	var out []Case
next:
	for _, c := range cases {
		if glob != "" {
			if ok, _ := path.Match(glob, c.ID); !ok {
				continue
			}
		}
		for _, t := range tags {
			found := false
			for _, ct := range c.Tags {
				found = found || ct == t
			}
			if !found {
				continue next
			}
		}
		out = append(out, c)
	}
	return out, nil
}
