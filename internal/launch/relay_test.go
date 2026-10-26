package launch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"labmill-client/internal/account"
	"labmill-client/internal/agent"
	"labmill-client/internal/agent/agenttest"
)

// fakeRelay is the Labmill test relay: POST /api/model/v1/messages with the
// program token, answered with one text reply as an event stream.
type fakeRelay struct {
	mu   sync.Mutex
	reqs []*http.Request
	body []map[string]any
}

func (f *fakeRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	json.Unmarshal(b, &body)
	f.mu.Lock()
	f.reqs, f.body = append(f.reqs, r), append(f.body, body)
	f.mu.Unlock()
	if r.URL.Path != "/api/model/v1/messages" {
		http.NotFound(w, r)
		return
	}
	sse, _ := agenttest.SSE(agenttest.TextReply("hello from the relay"))
	w.Header().Set("Content-Type", "text/event-stream")
	io.WriteString(w, sse)
}

func linkTo(t *testing.T, url string) string {
	path := filepath.Join(t.TempDir(), "account.json")
	if err := account.Save(path, account.Link{URL: url, Token: "lmt_" + strings.Repeat("t", 43), LinkedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestChooseModelRelay(t *testing.T) {
	f := &fakeRelay{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	t.Setenv("ANTHROPIC_API_KEY", "sk-must-not-leak") // read by chooseModel's caller only; the relay client ignores the environment
	m, path := chooseModel("", func(string) string { return "" }, linkTo(t, srv.URL))
	if path != PathRelay {
		t.Fatalf("path %q", path)
	}
	ag := &agent.Agent{Model: m, Tools: agent.NewRegistry(), NoThinking: true}
	if _, err := ag.Send(context.Background(), nil, "hi", agent.Typed); err != nil {
		t.Fatal(err)
	}
	if len(f.reqs) != 1 {
		t.Fatalf("%d requests", len(f.reqs))
	}
	r := f.reqs[0]
	if r.Header.Get("Authorization") != "Bearer lmt_"+strings.Repeat("t", 43) || r.Header.Get("X-Api-Key") != "" {
		t.Errorf("auth %q, x-api-key %q", r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"))
	}
	if r.Header.Get("X-Labmill-Task") != "plan" || f.body[0]["model"] != "labmill-broker" || f.body[0]["stream"] != true {
		t.Errorf("hint %q, model %v, stream %v", r.Header.Get("X-Labmill-Task"), f.body[0]["model"], f.body[0]["stream"])
	}
}

func TestChooseModelOwnKeyFirst(t *testing.T) {
	f := &fakeRelay{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	if _, path := chooseModel("sk-own", func(string) string { return "" }, linkTo(t, srv.URL)); path != PathOwnKey {
		t.Errorf("own key: path %q", path)
	}
	if len(f.reqs) != 0 {
		t.Error("the relay was called")
	}
}

func TestChooseModelNone(t *testing.T) {
	for _, linkPath := range []string{"", filepath.Join(t.TempDir(), "account.json")} {
		m, path := chooseModel("", func(string) string { return "" }, linkPath)
		if path != PathNone {
			t.Errorf("%q: path %q", linkPath, path)
		}
		_, _, err := m.BetaMessages()
		if err == nil || !strings.Contains(err.Error(), "labmill login") || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
			t.Errorf("%q: message %v", linkPath, err)
		}
	}
}
