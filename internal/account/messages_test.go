package account

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMessages serves GET /api/messages like the server: the oldest 50 messages newer than since.
type fakeMessages struct {
	mu    sync.Mutex
	all   []string // JSON objects, oldest first; each has created_at
	times []time.Time
	calls []string
}

func (f *fakeMessages) add(id any, body string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idj, _ := json.Marshal(id)
	bj, _ := json.Marshal(body)
	f.all = append(f.all, fmt.Sprintf(`{"id":%s,"body":%s,"tenant":"t1","created_at":%q,"help_request_id":7}`, idj, bj, at.UTC().Format(time.RFC3339)))
	f.times = append(f.times, at)
}

func (f *fakeMessages) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/messages" || r.Header.Get("Authorization") != "Bearer "+goodToken {
			t.Errorf("request %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		since, err := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
		if err != nil {
			http.Error(w, "bad since", 400)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.URL.RawQuery)
		var out []string
		for i, at := range f.times {
			if at.After(since) && len(out) < 50 {
				out = append(out, f.all[i])
			}
		}
		w.Write([]byte("[" + strings.Join(out, ",") + "]"))
	})
}

func msgReader(t *testing.T, url string, linked time.Time, got *[]string) *MessageReader {
	p := filepath.Join(t.TempDir(), "account.json")
	if err := Save(p, Link{URL: url, Token: goodToken, LinkedAt: linked}); err != nil {
		t.Fatal(err)
	}
	return &MessageReader{LinkPath: p, Notice: func(s string) { *got = append(*got, s) }}
}

func TestMessageReaderShowsEachReplyOnce(t *testing.T) {
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	f := &fakeMessages{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	var got []string
	r := msgReader(t, srv.URL, base.Add(-time.Hour), &got)
	if n, err := r.Pass(context.Background()); n != 0 || err != nil {
		t.Fatalf("empty: %d %v", n, err)
	}
	f.add(1, "Try --mem=8G", base)
	f.add("m2", "Also check the module\x1b[31m list", base.Add(time.Minute)) // a string id and a control code
	if n, err := r.Pass(context.Background()); n != 2 || err != nil {
		t.Fatalf("first: %d %v", n, err)
	}
	if len(got) != 2 || !strings.Contains(got[0], "research computing team") || !strings.Contains(got[0], "Try --mem=8G") || strings.Contains(got[1], "\x1b") {
		t.Fatalf("shown: %q", got)
	}
	// The same messages again (the overlap of the since time) are not shown twice, also after a restart.
	r2 := &MessageReader{LinkPath: r.LinkPath, Notice: r.Notice}
	f.add(3, "One more", base.Add(2*time.Minute))
	if n, err := r2.Pass(context.Background()); n != 1 || err != nil || len(got) != 3 || !strings.Contains(got[2], "One more") {
		t.Fatalf("second: %d %v %q", n, err, got)
	}
	if st, err := os.Stat(filepath.Join(filepath.Dir(r.LinkPath), "messages.json")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("state file: %v %v", st, err)
	}
}

func TestMessagesPageForward(t *testing.T) {
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	f := &fakeMessages{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	for i := 0; i < 120; i++ {
		f.add(i+1, fmt.Sprintf("m%d", i), base.Add(time.Duration(i)*10*time.Second))
	}
	ms, err := (&Client{URL: srv.URL, Token: goodToken}).Messages(context.Background(), base.Add(-time.Minute))
	if err != nil || len(ms) != 120 || ms[0].Body != "m0" || ms[119].Body != "m119" {
		t.Fatalf("%d messages, %v", len(ms), err)
	}
	if len(f.calls) != 3 {
		t.Errorf("calls %v", f.calls)
	}
}

func TestMessageReaderNeedsALinkAndReportsErrors(t *testing.T) {
	var got []string
	r := &MessageReader{LinkPath: filepath.Join(t.TempDir(), "account.json"), Notice: func(s string) { got = append(got, s) }}
	if n, err := r.Pass(context.Background()); n != 0 || err != nil {
		t.Fatalf("no link: %d %v", n, err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "", 401) }))
	defer srv.Close()
	r = msgReader(t, srv.URL, time.Now().Add(-time.Hour), &got)
	if _, err := r.Pass(context.Background()); err == nil || len(got) != 0 {
		t.Fatalf("revoked token: %v %q", err, got)
	}
}
