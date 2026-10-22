package app

import (
	"context"
	"strings"
	"testing"

	"labmill-client/internal/config"
)

// M59.1: an account name already used at the institution is tried first, for
// the home institution and for another one alike.
func TestAddClusterReusesKnownUsername(t *testing.T) {
	tests := []struct {
		name     string
		known    func(site string) (string, string)
		input    map[string]any
		wantUser string // "" means an error asking for the name
		wantFrom string
	}{
		{"other site, known there", func(s string) (string, string) {
			if s == "far.example.org" {
				return "farhat", "farother"
			}
			return "", ""
		}, map[string]any{"site": "far.example.org", "cluster": "farone"}, "farhat", "farother"},
		{"explicit name wins", func(string) (string, string) { return "farhat", "farother" },
			map[string]any{"site": "far.example.org", "cluster": "farone", "username": "bob"}, "bob", ""},
		{"nothing known", func(string) (string, string) { return "", "" },
			map[string]any{"site": "far.example.org", "cluster": "farone"}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, applied := addClusterHarness(t)
			h.env.KnownUsername = tt.known
			p, err := h.tools["add_cluster"].Propose(mustJSON(tt.input))
			if tt.wantUser == "" {
				if err == nil || !strings.Contains(err.Error(), "Ask the user for their account name") {
					t.Fatalf("err = %v, want a request to ask for the name", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(p.Summary, "account "+tt.wantUser) {
				t.Errorf("summary = %q", p.Summary)
			}
			if tt.wantFrom != "" && (!strings.Contains(p.Details, "(the same as on "+tt.wantFrom+")") || !strings.Contains(p.Details, "set_cluster_account")) {
				t.Errorf("details do not name the source:\n%s", p.Details)
			}
			if tt.wantFrom == "" && strings.Contains(p.Details, "the same as on") {
				t.Errorf("a typed name is not a reused one:\n%s", p.Details)
			}
			out, err := h.run(t, "add_cluster", tt.input)
			if err != nil {
				t.Fatal(err)
			}
			if len(*applied) != 1 || (*applied)[0] != tt.wantUser {
				t.Errorf("applied = %v", *applied)
			}
			if tt.wantFrom != "" && !strings.Contains(out, "reused from "+tt.wantFrom) {
				t.Errorf("result = %q", out)
			}
		})
	}
}

// A name filled in automatically yields to the account of a reused SSH entry
// instead of raising a conflict over a name the user never typed.
func TestAddClusterAutoNameYieldsToReusedEntry(t *testing.T) {
	h, applied := addClusterHarness(t)
	h.env.KnownUsername = func(string) (string, string) { return "alice", "hpcone" }
	old := h.env.PlanAddCluster
	h.env.PlanAddCluster = func(s, c string) (AddClusterPlan, error) {
		p, err := old(s, c)
		p.Hosts[0].ReusedAlias, p.Hosts[0].ReusedUser = "mine", "carol"
		return p, err
	}
	in := map[string]any{"site": "far.example.org", "cluster": "farone"}
	p, err := h.tools["add_cluster"].Propose(mustJSON(in))
	if err != nil || !strings.Contains(p.Summary, "account carol") || !strings.Contains(p.Details, "existing SSH entry mine") {
		t.Fatalf("proposal = %+v, %v", p, err)
	}
	if _, err := h.run(t, "add_cluster", in); err != nil || len(*applied) != 1 || (*applied)[0] != "carol" {
		t.Errorf("applied = %v, %v", *applied, err)
	}
	// A typed name that differs is still a conflict.
	in["username"] = "alice"
	if _, err := h.tools["add_cluster"].Propose(mustJSON(in)); err == nil {
		t.Error("typed conflicting name accepted")
	}
}

// The institution and cluster are checked before the account name is asked.
func TestAddClusterChecksSiteBeforeName(t *testing.T) {
	h, _ := addClusterHarness(t)
	h.env.KnownUsername = func(string) (string, string) { return "", "" }
	_, err := h.tools["add_cluster"].Propose(mustJSON(map[string]any{"site": "nowhere.example.com", "cluster": "x"}))
	if err == nil || !strings.Contains(err.Error(), "no profile") {
		t.Errorf("err = %v", err)
	}
}

func TestAddClusterDescriptionConsistent(t *testing.T) {
	h := newHarness(t)
	d := h.tools["add_cluster"].Description
	if strings.Contains(d, "account name at that institution first") {
		t.Errorf("description still tells the model to ask first: %s", d)
	}
	if !strings.Contains(d, "Leave username empty") {
		t.Errorf("description lacks the reuse rule: %s", d)
	}
}

type accountFake struct {
	planErr error
	set     []string
}

func accountHarness(t *testing.T) (*harness, *accountFake) {
	t.Helper()
	h := newHarness(t)
	f := &accountFake{}
	h.env.PlanClusterAccount = func(c config.Cluster, username string) (AccountChange, error) {
		if f.planErr != nil {
			return AccountChange{}, f.planErr
		}
		return AccountChange{File: "/home/u/.ssh/config.d/labmill", Lines: []string{c.SSHHost + ": User alice -> " + username}}, nil
	}
	h.env.SetClusterAccount = func(_ context.Context, c config.Cluster, username string) ([]string, error) {
		f.set = append(f.set, c.Name+":"+username)
		return []string{c.SSHHost}, nil
	}
	return h, f
}

func TestSetClusterAccount(t *testing.T) {
	h, f := accountHarness(t)
	tool := h.tools["set_cluster_account"]
	in := map[string]any{"cluster": "hpcone", "username": "bob"}
	p, err := tool.Propose(mustJSON(in))
	if err != nil {
		t.Fatal(err)
	}
	if !tool.NeedsApproval || !p.Secret || !strings.Contains(p.Details, "User alice -> bob") || !strings.Contains(p.Details, "config.d/labmill") {
		t.Errorf("proposal = %+v", p)
	}
	if len(f.set) != 0 {
		t.Fatal("Propose changed something")
	}
	if _, err := h.run(t, "set_cluster_account", in); err != nil {
		t.Fatal(err)
	}
	if len(f.set) != 1 || f.set[0] != "hpcone:bob" {
		t.Errorf("set = %v", f.set)
	}
}

func TestSetClusterAccountRefusals(t *testing.T) {
	tests := []struct {
		name    string
		input   map[string]any
		planErr error
		want    string
	}{
		{"bad name", map[string]any{"cluster": "hpcone", "username": "Al Ice"}, nil, "not valid"},
		{"no name", map[string]any{"cluster": "hpcone"}, nil, "not valid"},
		{"no cluster", map[string]any{"username": "bob"}, nil, "needs the cluster"},
		{"unknown cluster", map[string]any{"cluster": "nope", "username": "bob"}, nil, "No cluster named"},
		{"user's own entry", map[string]any{"cluster": "hpcone", "username": "bob"}, errString("is in the user's own SSH settings"), "own SSH settings"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, f := accountHarness(t)
			f.planErr = tt.planErr
			_, err := h.run(t, "set_cluster_account", tt.input)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
			if len(f.set) != 0 {
				t.Errorf("changed %v", f.set)
			}
		})
	}
}

type errString string

func (e errString) Error() string { return string(e) }
