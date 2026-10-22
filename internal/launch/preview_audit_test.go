package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"labmill-client/internal/agent"
	"labmill-client/internal/agent/agenttest"
	"labmill-client/internal/app"
	"labmill-client/internal/config"
	"labmill-client/internal/store"
)

// missingFileMarker is the text of preview_file's answer for a path that
// doesn't exist (M38.1, internal/app missingFile: "<name> doesn't exist in
// <dir>; list the folder (find_local_data) instead of guessing names.").
// TestPreviewAuditRealTool pins that the real tool still says it.
const missingFileMarker = "instead of guessing names"

// previewAudit watches an agent's events for guessed file names (M38.5).
//
// The hard check is "no preview_file call was answered with the missing-file
// message": the tool itself is the authority on whether the file exists, so
// the check has no false positives. The stricter "every previewed path was
// listed earlier" is only counted (Unlisted) and not enforced: a README,
// the system prompt or a chosen folder's listing legitimately name files
// that no find_local_data result shows (its lists are capped), and relative
// versus absolute spellings would make a literal match brittle.
type previewAudit struct {
	mu       sync.Mutex
	pending  []string // preview paths whose result hasn't arrived
	listed   strings.Builder
	Previews int
	Missing  []string // previewed paths answered with the missing-file message
	Unlisted int      // previews whose file name no earlier find_local_data or stage_data result shows
}

// Observe is an agent.Agent OnEvent hook.
func (a *previewAudit) Observe(e agent.Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case e.Kind == agent.EventToolCall && e.Tool == "preview_file":
		var in struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(e.Input, &in)
		a.pending = append(a.pending, in.Path)
		a.Previews++
		base := in.Path
		if i := strings.LastIndexAny(base, `/\`); i >= 0 {
			base = base[i+1:]
		}
		if !strings.Contains(a.listed.String(), base) {
			a.Unlisted++
		}
	case e.Kind == agent.EventToolResult && e.Tool == "preview_file":
		p := ""
		if len(a.pending) > 0 {
			p, a.pending = a.pending[0], a.pending[1:]
		}
		if e.IsError && strings.Contains(e.Text, missingFileMarker) {
			a.Missing = append(a.Missing, p)
		}
	case e.Kind == agent.EventToolResult && (e.Tool == "find_local_data" || e.Tool == "stage_data"):
		a.listed.WriteString(e.Text)
		a.listed.WriteByte('\n')
	}
}

// Check returns an error naming the guessed files, or nil.
func (a *previewAudit) Check() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.Missing) > 0 {
		return fmt.Errorf("%d of %d previews were of files that don't exist (guessed names): %s", len(a.Missing), a.Previews, strings.Join(a.Missing, ", "))
	}
	return nil
}

// Summary is the line for the test log.
func (a *previewAudit) Summary() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return fmt.Sprintf("preview_file: %d calls, %d of missing files, %d of files no earlier listing showed", a.Previews, len(a.Missing), a.Unlisted)
}

// TestPreviewAuditRealTool runs a scripted model against the real
// find_local_data and preview_file tools: previewing a nonexistent name must
// fail the audit, previewing listed files must pass.
func TestPreviewAuditRealTool(t *testing.T) {
	run := func(t *testing.T, previews ...string) *previewAudit {
		t.Helper()
		base := t.TempDir()
		code := filepath.Join(base, "code")
		if err := os.MkdirAll(code, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{"README.md": "# demo\nBuild with cmake.\n", "CMakeLists.txt": "project(demo)\n"} {
			if err := os.WriteFile(filepath.Join(code, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		st, err := store.Open(filepath.Join(t.TempDir(), "y.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		ws := app.NewWorkspace(base, "")
		if _, err := ws.SetFolder(app.FolderCode, code); err != nil {
			t.Fatalf("code folder: %v", err)
		}
		env := &app.Env{
			Config:     &config.Config{},
			Store:      st,
			LocalRoots: []string{base},
			Workspace:  ws,
			ResultsDir: filepath.Join(base, "results"),
			Now:        time.Now,
		}
		reg := agent.NewRegistry()
		for _, tl := range app.Tools(env) {
			if err := reg.Register(tl); err != nil {
				t.Fatal(err)
			}
		}
		replies := []string{agenttest.ToolReply(agenttest.ToolUse{ID: "l1", Name: "find_local_data", Input: fmt.Sprintf(`{"query":"code","under":%q}`, base)})}
		for i, p := range previews {
			in, _ := json.Marshal(map[string]string{"path": p})
			replies = append(replies, agenttest.ToolReply(agenttest.ToolUse{ID: fmt.Sprintf("p%d", i), Name: "preview_file", Input: string(in)}))
		}
		replies = append(replies, agenttest.TextReply("done"))
		b := agenttest.New(t, replies...)
		audit := &previewAudit{}
		ag := &agent.Agent{Model: b.Client(t), Tools: reg, OnEvent: audit.Observe, MaxTurns: 20}
		if _, err := ag.Send(context.Background(), nil, "run this example on the GPU cluster", agent.Typed); err != nil {
			t.Fatal(err)
		}
		t.Log(audit.Summary())
		return audit
	}

	t.Run("guessed names fail", func(t *testing.T) {
		a := run(t, "run.sh", "build.sh")
		if err := a.Check(); err == nil {
			t.Fatalf("audit passed a run that previewed nonexistent files: %s", a.Summary())
		} else if len(a.Missing) != 2 {
			t.Errorf("missing = %v, want 2 entries (%v)", a.Missing, err)
		}
	})
	t.Run("listed files pass", func(t *testing.T) {
		a := run(t, "README.md", "CMakeLists.txt")
		if err := a.Check(); err != nil {
			t.Fatal(err)
		}
		if a.Previews != 2 {
			t.Errorf("previews = %d, want 2", a.Previews)
		}
		if a.Unlisted != 0 {
			t.Errorf("unlisted = %d, want 0 (the listing shows both)", a.Unlisted)
		}
	})
}
