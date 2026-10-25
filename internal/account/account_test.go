package account

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

const goodToken = "lmt_abcdefghijklmnopqrstuvwxyz012345"

// fakeAPI is the account API: /api/me, /api/systems, /api/jobs.
type fakeAPI struct {
	mu       sync.Mutex
	jobs     []string // raw bodies of POST /api/jobs
	refs     map[string]bool
	jobFails int // answer 500 to this many job posts first
	jobCode  int // when set, answer every job post with it
	requests int
	// resolve answers GET /api/systems/resolve; nil: 404 (an older server).
	resolve  func(q url.Values) (int, any)
	resolved []url.Values
}

func (f *fakeAPI) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests++
		if r.Header.Get("Authorization") != "Bearer "+goodToken {
			http.Error(w, "sign in first", http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/me":
			json.NewEncoder(w).Encode(Me{PersonID: "11111111-2222-3333-4444-555555555555", Role: "user", DisplayName: "Test User"})
		case "GET /api/systems":
			json.NewEncoder(w).Encode([]System{
				{ID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Name: "bigcluster", Institution: "Example University"},
				{ID: "ffffffff-bbbb-cccc-dddd-eeeeeeeeeeee", Name: "bigcluster", Institution: "Other College"},
				{ID: "99999999-bbbb-cccc-dddd-eeeeeeeeeeee", Name: "bigcluster2", Institution: "Example University"},
			})
		case "GET /api/systems/resolve":
			f.resolved = append(f.resolved, r.URL.Query())
			if f.resolve == nil {
				http.NotFound(w, r)
				return
			}
			code, body := f.resolve(r.URL.Query())
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(body)
		case "POST /api/jobs":
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("content type %q", r.Header.Get("Content-Type"))
			}
			b, _ := io.ReadAll(r.Body)
			if f.jobCode != 0 {
				http.Error(w, "bad field: state", f.jobCode)
				return
			}
			if f.jobFails > 0 {
				f.jobFails--
				http.Error(w, "could not store the job", http.StatusInternalServerError)
				return
			}
			var j map[string]any
			if err := json.Unmarshal(b, &j); err != nil {
				t.Errorf("bad body: %v", err)
			}
			ref, _ := j["client_job_ref"].(string)
			if f.refs == nil {
				f.refs = map[string]bool{}
			}
			if !f.refs[ref] { // the API's ON CONFLICT DO NOTHING
				f.refs[ref] = true
				f.jobs = append(f.jobs, string(b))
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
}

func (f *fakeAPI) count() (jobs, requests int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.jobs), f.requests
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "labmill.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// addJob makes a submitted job that ran and ended in state end (message msg).
func addJob(t *testing.T, st *store.Store, end store.JobState, msg string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := st.CreateJob(ctx, store.Job{Name: "secret-project-name", Cluster: "bigcluster", Backend: "slurm",
		Script: "#!/bin/bash\nexport API_KEY=sk-live-0123456789abcdef\npython train.py", RemoteDir: "/data/mygroup/private-project",
		LocalDir: "/home/someone/labmill-results/secret-project-name"})
	if err != nil {
		t.Fatal(err)
	}
	spec := backend.JobSpec{Name: "secret-project-name", Partition: "gpu", GPUs: 1, GPUType: "a100", CPUs: 8, MemoryGB: 32,
		Time: 12 * time.Hour, Command: "python train.py --password hunter2", WorkDir: "/data/mygroup/private-project",
		Env:         map[string]string{"HF_TOKEN": "hf_supersecretvalue"},
		Environment: &backend.Environment{Modules: []string{"python/3.12", "/home/someone/modules/mytool", "cuda/12.4"}}}
	b, _ := json.Marshal(spec)
	if err := st.AddEvent(ctx, id, "spec", string(b)); err != nil {
		t.Fatal(err)
	}
	for _, s := range []store.JobState{store.StateSubmitted, store.StateRunning, end} {
		err := st.UpdateJob(ctx, id, func(j *store.Job) error {
			j.State, j.RemoteID = s, "4242"
			if s == end {
				j.Message = msg
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func link(t *testing.T, url string, at time.Time) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "labmill", "account.json")
	if err := Save(p, Link{URL: url, Token: goodToken, LinkedAt: at}); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoginWhoamiLogout(t *testing.T) {
	f := &fakeAPI{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "labmill", "account.json")

	if _, err := Login(ctx, p, srv.URL, "lmt_wrongwrongwrongwrongwrong", nil, time.Now()); !errors.Is(err, ErrRejected) {
		t.Fatalf("wrong token: err = %v", err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a rejected token was saved")
	}
	if _, err := Login(ctx, p, srv.URL, "not-a-token", nil, time.Now()); err == nil {
		t.Fatal("a malformed token was accepted")
	}
	me, err := Login(ctx, p, srv.URL, "  "+goodToken+"\n", nil, time.Now())
	if err != nil || me.DisplayName != "Test User" {
		t.Fatalf("login: %+v %v", me, err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("account.json mode %v, want owner only", fi.Mode().Perm())
	}
	me, l, err := Whoami(ctx, p, nil)
	if err != nil || me.PersonID == "" || l.Token != goodToken {
		t.Fatalf("whoami: %+v %v", me, err)
	}
	if err := Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Whoami(ctx, p, nil); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("after logout: %v", err)
	}
	if err := Remove(p); err != nil {
		t.Fatalf("second logout: %v", err)
	}
}

func TestCheckURL(t *testing.T) {
	for u, ok := range map[string]bool{
		"https://app.labmill.org": true, "http://127.0.0.1:8080": true, "http://localhost:3000": true,
		"http://app.labmill.org": false, "ftp://x": false, "app.labmill.org": false,
	} {
		if err := CheckURL(u); (err == nil) != ok {
			t.Errorf("CheckURL(%q) = %v", u, err)
		}
	}
}

func TestUploadSendsSchemaWithoutPrivateData(t *testing.T) {
	f := &fakeAPI{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	st := newStore(t)
	p := link(t, srv.URL, time.Now().Add(-time.Minute))
	addJob(t, st, store.StateFailed, "OUT_OF_MEMORY")
	u := &Uploader{Store: st, LinkPath: p, Institution: func(string) string { return "Example University" }}
	n, err := u.Pass(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("pass: %d %v", n, err)
	}
	body := f.jobs[0]
	for _, bad := range []string{"secret-project-name", "private-project", "mygroup", "train.py", "hunter2", "HF_TOKEN",
		"hf_supersecret", "sk-live", "#!/bin/bash", "someone", "mytool", "log_lines", "redact_words", "script"} {
		if strings.Contains(body, bad) {
			t.Errorf("report holds %q: %s", bad, body)
		}
	}
	var got map[string]any
	json.Unmarshal([]byte(body), &got)
	want := map[string]any{"hpc_system_id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "backend": "slurm", "state": "OUT_OF_MEMORY",
		"failure_category": "out_of_memory", "partition": "gpu"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if ref, _ := got["client_job_ref"].(string); len(ref) < 8 || len(ref) > 64 {
		t.Errorf("client_job_ref %q", ref)
	}
	req, _ := got["req"].(map[string]any)
	if req["cpu"] != 8.0 || req["mem"] != "32G" || req["gpu"] != 1.0 || req["gpu_type"] != "a100" || req["time"] != "12:00:00" {
		t.Errorf("req = %v", req)
	}
	sw, _ := got["software"].([]any)
	if len(sw) != 2 {
		t.Errorf("software = %v", sw)
	}
	for _, k := range []string{"submitted_at", "started_at", "ended_at", "queue_wait_sec"} {
		if _, ok := got[k]; !ok {
			t.Errorf("%s missing", k)
		}
	}
	// The API refuses unknown fields: every key must be one it knows.
	known := "client_job_ref hpc_system_id tenant_id backend state exit_code failure_category submitted_at started_at ended_at req used partition constraints queue_wait_sec domain_field shared software log_lines redact_words"
	for k := range got {
		if !strings.Contains(" "+known+" ", " "+k+" ") {
			t.Errorf("unknown field %s", k)
		}
	}

	// No duplicates: a second pass sends nothing.
	if n, err := u.Pass(context.Background()); err != nil || n != 0 {
		t.Fatalf("second pass: %d %v", n, err)
	}
	sent, waiting, refused, _, err := Queue(context.Background(), st)
	if err != nil || sent != 1 || waiting != 0 || refused != 0 {
		t.Errorf("queue: %d %d %d %v", sent, waiting, refused, err)
	}
}

func TestNothingSentWhenLoggedOut(t *testing.T) {
	f := &fakeAPI{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	st := newStore(t)
	addJob(t, st, store.StateCompleted, "")
	u := &Uploader{Store: st, LinkPath: filepath.Join(t.TempDir(), "account.json")}
	if n, err := u.Pass(context.Background()); err != nil || n != 0 {
		t.Fatalf("pass: %d %v", n, err)
	}
	if _, reqs := f.count(); reqs != 0 {
		t.Errorf("%d requests while logged out", reqs)
	}
	if ups, _ := st.Uploads(context.Background()); len(ups) != 0 {
		t.Errorf("queued %d reports while logged out", len(ups))
	}
}

func TestJobsBeforeLinkNotSent(t *testing.T) {
	f := &fakeAPI{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	st := newStore(t)
	addJob(t, st, store.StateCompleted, "")
	p := link(t, srv.URL, time.Now().Add(time.Second))
	time.Sleep(1100 * time.Millisecond)
	// The old job is saved again after the link (results fetched): still not sent.
	st.UpdateJob(context.Background(), 1, func(j *store.Job) error { j.State = store.StateFetched; return nil })
	u := &Uploader{Store: st, LinkPath: p}
	if n, err := u.Pass(context.Background()); err != nil || n != 0 {
		t.Fatalf("pass: %d %v", n, err)
	}
}

func TestRetryWithBackoff(t *testing.T) {
	f := &fakeAPI{jobFails: 1}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	st := newStore(t)
	p := link(t, srv.URL, time.Now().Add(-time.Minute))
	addJob(t, st, store.StateCompleted, "")
	now := time.Now()
	u := &Uploader{Store: st, LinkPath: p, Now: func() time.Time { return now }, Institution: func(string) string { return "example university" }}
	if n, _ := u.Pass(context.Background()); n != 0 {
		t.Fatalf("sent %d despite a server error", n)
	}
	_, waiting, _, lastErr, _ := Queue(context.Background(), st)
	if waiting != 1 || !strings.Contains(lastErr, "500") {
		t.Fatalf("waiting %d, last error %q", waiting, lastErr)
	}
	if n, _ := u.Pass(context.Background()); n != 0 {
		t.Fatal("retried before the backoff ended")
	}
	now = now.Add(Backoff(1) + time.Second)
	if n, err := u.Pass(context.Background()); n != 1 || err != nil {
		t.Fatalf("retry: %d %v", n, err)
	}
	if jobs, _ := f.count(); jobs != 1 {
		t.Errorf("server has %d jobs", jobs)
	}
}

func TestRefusedAndRejected(t *testing.T) {
	f := &fakeAPI{jobCode: http.StatusBadRequest}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	st := newStore(t)
	p := link(t, srv.URL, time.Now().Add(-time.Minute))
	addJob(t, st, store.StateCanceled, "")
	u := &Uploader{Store: st, LinkPath: p, Institution: func(string) string { return "Example University" }}
	u.Pass(context.Background())
	if _, _, refused, _, _ := Queue(context.Background(), st); refused != 1 {
		t.Errorf("a 400 is not final: refused = %d", refused)
	}

	f2 := &fakeAPI{}
	srv2 := httptest.NewServer(f2.handler(t))
	defer srv2.Close()
	st2 := newStore(t)
	p2 := filepath.Join(t.TempDir(), "account.json")
	Save(p2, Link{URL: srv2.URL, Token: "lmt_revokedrevokedrevokedrevoked", LinkedAt: time.Now().Add(-time.Minute)})
	addJob(t, st2, store.StateCompleted, "")
	u2 := &Uploader{Store: st2, LinkPath: p2}
	if _, err := u2.Pass(context.Background()); !errors.Is(err, ErrRejected) {
		t.Errorf("revoked token: %v", err)
	}
	if _, waiting, _, _, _ := Queue(context.Background(), st2); waiting != 1 {
		t.Errorf("the report must wait for a new token: waiting = %d", waiting)
	}
}

func TestUnknownSystemWaits(t *testing.T) {
	f := &fakeAPI{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	st := newStore(t)
	p := link(t, srv.URL, time.Now().Add(-time.Minute))
	addJob(t, st, store.StateCompleted, "")
	// Two systems are named bigcluster: without the institution it is not unique.
	u := &Uploader{Store: st, LinkPath: p}
	if n, _ := u.Pass(context.Background()); n != 0 {
		t.Fatal("sent with an ambiguous system")
	}
	if _, waiting, _, lastErr, _ := Queue(context.Background(), st); waiting != 1 || !strings.Contains(lastErr, "doesn't know") {
		t.Errorf("waiting %d, %q", waiting, lastErr)
	}
}

func TestBackoff(t *testing.T) {
	if Backoff(1) != time.Minute || Backoff(2) != 2*time.Minute || Backoff(30) != 6*time.Hour {
		t.Errorf("backoff %v %v %v", Backoff(1), Backoff(2), Backoff(30))
	}
}

func TestBuildReportStates(t *testing.T) {
	for _, c := range []struct {
		st       store.JobState
		msg      string
		want     string
		category string
	}{
		{store.StateFetched, "", "COMPLETED", ""},
		{store.StateFailed, "TIMEOUT", "TIMEOUT", "timeout"},
		{store.StateFailed, "NODE_FAIL", "NODE_FAIL", ""},
		{store.StateFailed, "exit 1 in /home/x/run.sh", "FAILED", ""},
		{store.StateCanceled, "", "CANCELLED", ""},
	} {
		in, ok := BuildReport(store.Job{State: c.st, Message: c.msg, RemoteID: "1"}, nil, "lmc-x")
		if !ok || in.State != c.want || in.FailureCategory != c.category {
			t.Errorf("%s %q: %+v", c.st, c.msg, in)
		}
	}
	if _, ok := BuildReport(store.Job{State: store.StateRunning, RemoteID: "1"}, nil, "r"); ok {
		t.Error("a running job made a report")
	}
	if _, ok := BuildReport(store.Job{State: store.StateCanceled}, nil, "r"); ok {
		t.Error("a never-submitted job made a report")
	}
}

func TestResolveFirstThenSearch(t *testing.T) {
	const resolved = "12345678-bbbb-cccc-dddd-eeeeeeeeeeee"
	ref := func(string) SystemRef {
		return SystemRef{Host: "login1.example.edu", Name: "bigcluster", Domain: "example.edu"}
	}
	for _, c := range []struct {
		what   string
		code   int
		body   any
		inst   string
		wantID string // "" means nothing sent
	}{
		{"resolve match", 200, System{ID: resolved, Name: "bigcluster", Institution: "Example University"}, "", resolved},
		{"409 falls back to the search", 409, map[string]any{"candidates": []System{{ID: "a"}, {ID: "b"}}}, "Example University", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"},
		{"404 falls back to the search", 404, "no system matches", "Example University", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"},
		{"400 falls back to the search", 400, "bad field: host", "Example University", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"},
		{"409 and an ambiguous search: the report waits", 409, map[string]any{"candidates": []System{}}, "", ""},
		{"500: the report waits, no search", 500, "lookup failed", "Example University", ""},
	} {
		t.Run(c.what, func(t *testing.T) {
			f := &fakeAPI{resolve: func(url.Values) (int, any) { return c.code, c.body }}
			srv := httptest.NewServer(f.handler(t))
			defer srv.Close()
			st := newStore(t)
			p := link(t, srv.URL, time.Now().Add(-time.Minute))
			addJob(t, st, store.StateCompleted, "")
			u := &Uploader{Store: st, LinkPath: p, Ref: ref, Institution: func(string) string { return c.inst }}
			n, _ := u.Pass(context.Background())
			if len(f.resolved) != 1 || f.resolved[0].Get("host") != "login1.example.edu" ||
				f.resolved[0].Get("name") != "bigcluster" || f.resolved[0].Get("domain") != "example.edu" {
				t.Fatalf("resolve query: %v", f.resolved)
			}
			if c.wantID == "" {
				if n != 0 {
					t.Fatalf("sent %d", n)
				}
				if _, waiting, _, _, _ := Queue(context.Background(), st); waiting != 1 {
					t.Errorf("waiting %d, want 1", waiting)
				}
				return
			}
			if n != 1 || !strings.Contains(f.jobs[0], `"hpc_system_id":"`+c.wantID+`"`) {
				t.Fatalf("sent %d: %v", n, f.jobs)
			}
			// The answer is kept: a second report needs no second lookup.
			addJob(t, st, store.StateFailed, "")
			if n, err := u.Pass(context.Background()); n != 1 || err != nil || len(f.resolved) != 1 {
				t.Errorf("second pass: %d %v, %d lookups", n, err, len(f.resolved))
			}
		})
	}
}

func TestResolveQueryLeavesOutEmptyFields(t *testing.T) {
	f := &fakeAPI{resolve: func(url.Values) (int, any) { return 200, System{ID: "x"} }}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	c := &Client{URL: srv.URL, Token: goodToken}
	if s, err := c.Resolve(context.Background(), SystemRef{Name: "bigcluster"}); err != nil || s.ID != "x" {
		t.Fatalf("resolve: %+v %v", s, err)
	}
	if q := f.resolved[0]; len(q) != 1 || q.Get("name") != "bigcluster" {
		t.Errorf("query %v", q)
	}
}
