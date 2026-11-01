package launch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/user"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"labmill-client/internal/account"
	"labmill-client/internal/agent"
	"labmill-client/internal/config"
	"labmill-client/internal/sshconfig"
	"labmill-client/internal/store"
)

// reportWords are the names only this computer knows, redacted from job
// reports and problem reports (account.Redactor): the local login name, the
// clusters' SSH aliases, host names and user names, and the last parts of
// job j's folders (j may be the zero Job).
func reportWords(cfg *config.Config, j store.Job) []string {
	var w []string
	if u, err := user.Current(); err == nil {
		w = append(w, u.Username, filepath.Base(u.HomeDir))
		if _, name, ok := strings.Cut(u.Username, `\`); ok { // Windows DOMAIN\name
			w = append(w, name)
		}
	}
	paths, perr := sshconfig.DefaultPaths()
	if cfg != nil {
		for _, c := range cfg.Clusters {
			for _, alias := range []string{c.SSHHost, c.FilesHost} {
				if alias == "" {
					continue
				}
				w = append(w, alias)
				if perr == nil {
					if h, _, err := sshconfig.Lookup(paths, alias); err == nil {
						w = append(w, h.HostName, h.User)
					}
				}
			}
		}
	}
	for _, d := range []string{j.RemoteDir, filepath.ToSlash(j.LocalDir)} {
		if d == "" {
			continue
		}
		w = append(w, path.Base(d), path.Base(path.Dir(d)))
	}
	return w
}

// problemTracker turns the agent's failed tool calls into problem reports
// (account.Problems). It remembers each tool call's cluster and job_id until
// its result arrives.
type problemTracker struct {
	p    *account.Problems
	mu   sync.Mutex
	last map[string]json.RawMessage // tool -> input of its latest call
}

func (t *problemTracker) observe(e agent.Event) {
	if t == nil || t.p == nil {
		return
	}
	switch {
	case e.Kind == agent.EventToolCall:
		t.mu.Lock()
		if t.last == nil {
			t.last = map[string]json.RawMessage{}
		}
		t.last[e.Tool] = e.Input
		t.mu.Unlock()
	case e.Kind == agent.EventToolResult && e.IsError && !strings.HasPrefix(e.Text, "The user declined"):
		t.mu.Lock()
		in := t.last[e.Tool]
		t.mu.Unlock()
		var args struct {
			Cluster string `json:"cluster"`
			JobID   int64  `json:"job_id"`
		}
		_ = json.Unmarshal(in, &args)
		go t.p.Record(account.ToolKind(e.Tool, e.Text), e.Tool, args.Cluster, args.JobID, e.Text)
	}
}

// turnEnded records a turn's error: the user stopped it, or the model
// (the API, the broker or the relay) failed.
func (t *problemTracker) turnEnded(err error) {
	if t == nil || t.p == nil || err == nil {
		return
	}
	if errors.Is(err, context.Canceled) {
		go t.p.Record(account.KindCancel, "", "", 0, "the user stopped the turn")
		return
	}
	go t.p.Record(account.KindModel, "", "", 0, err.Error())
}

// crashed records a recovered panic with the stack frames of Labmill's own
// code only (function and file:line, no folders, no arguments).
func (t *problemTracker) crashed(r any, stack []byte) {
	if t == nil || t.p == nil {
		return
	}
	t.p.Record(account.KindCrash, "", "", 0, fmt.Sprintf("panic: %v\n%s", r, ownFrames(string(stack))))
}

// ownFrames keeps the frames of a goroutine stack (debug.Stack) whose
// function is in this module: "labmill-client/internal/app.(*toolset).run
// tools.go:120".
func ownFrames(stack string) string {
	lines := strings.Split(stack, "\n")
	var out []string
	for i := 0; i+1 < len(lines); i++ {
		fn := lines[i]
		if !strings.HasPrefix(fn, "labmill-client/") {
			continue
		}
		if k := strings.LastIndex(fn, "("); k > 0 && strings.HasSuffix(fn, ")") {
			fn = fn[:k]
		}
		loc := strings.TrimSpace(lines[i+1])
		if k := strings.LastIndex(loc, " +0x"); k > 0 {
			loc = loc[:k]
		}
		out = append(out, fn+" "+path.Base(filepath.ToSlash(loc)))
		i++
		if len(out) == 30 {
			break
		}
	}
	return strings.Join(out, "\n")
}

// newProblems is the session's problem recorder (nil without an account path).
func newProblems(db *store.Store, cfg *config.Config) *problemTracker {
	p, err := account.Path()
	if err != nil || db == nil {
		return nil
	}
	return &problemTracker{p: &account.Problems{Store: db, LinkPath: p, Version: Version, OS: runtime.GOOS,
		Words: func(j store.Job) []string { return reportWords(cfg, j) }}}
}
