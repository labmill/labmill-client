package testrun

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/account"
	"labmill-client/internal/testcase"
)

func done(class, should string, kw ...string) testcase.Case {
	return testcase.Case{Expect: testcase.Expect{Outcome: "failed", FailureClass: class, AgentShould: should, AgentKeywords: kw}}
}

func TestClass(t *testing.T) {
	tests := []struct {
		name string
		o    Observation
		want string
	}{
		{"completed", Observation{JobState: "COMPLETED"}, ""},
		{"rejected", Observation{SubmitErr: "invalid partition"}, "submit_rejected"},
		{"timeout", Observation{JobState: "TIMEOUT", ExitCode: 1}, "timeout"},
		{"oom state", Observation{JobState: "OUT_OF_MEMORY"}, "out_of_memory"},
		{"oom tail", Observation{JobState: "FAILED", ExitCode: 137, OutputTail: "slurmstepd: Detected 1 oom-kill event"}, "out_of_memory"},
		{"missing", Observation{JobState: "FAILED", ExitCode: 127, OutputTail: "bwa: command not found"}, "missing_software"},
		{"py module", Observation{JobState: "FAILED", ExitCode: 1, OutputTail: "ModuleNotFoundError: No module named 'x'"}, "missing_software"},
		{"data", Observation{JobState: "FAILED", ExitCode: 1, OutputTail: "cat: in.csv: No such file or directory"}, "data_missing"},
		{"exit", Observation{JobState: "FAILED", ExitCode: 1, OutputTail: "bye"}, "exit_nonzero"},
	}
	for _, tt := range tests {
		if got := Class(tt.o); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestJudge(t *testing.T) {
	ok := testcase.Case{Expect: testcase.Expect{Outcome: "completed"}}
	tests := []struct {
		name    string
		c       testcase.Case
		o       Observation
		outcome string
		reason  string
	}{
		{"pass", ok, Observation{JobState: "COMPLETED"}, Pass, ""},
		{"wrong", ok, Observation{JobState: "COMPLETED", CheckExit: 1}, Fail, "wrong_result"},
		{"job failed", ok, Observation{JobState: "TIMEOUT"}, Fail, "job_failed:timeout"},
		{"job failed unknown", ok, Observation{JobState: "CANCELLED"}, Fail, "job_failed:unknown"},
		{"explain ok", done("exit_nonzero", "explain", "exit"), Observation{JobState: "FAILED", ExitCode: 1, FinalText: "The job ended with Exit code 1"}, Pass, ""},
		{"explain no word", done("exit_nonzero", "explain", "exit"), Observation{JobState: "FAILED", ExitCode: 1, FinalText: "done"}, Fail, "no keyword in final message"},
		{"wrong class", done("timeout", "explain", "time"), Observation{JobState: "FAILED", ExitCode: 1}, Fail, `class "exit_nonzero", want "timeout"`},
		{"unexpected complete", done("timeout", "explain", "time"), Observation{JobState: "COMPLETED"}, Fail, "job_completed"},
		{"resubmit ok", done("out_of_memory", "fix_and_resubmit"), Observation{JobState: "OUT_OF_MEMORY", ResubmitCompleted: true}, Pass, ""},
		{"resubmit missing", done("out_of_memory", "fix_and_resubmit"), Observation{JobState: "OUT_OF_MEMORY"}, Fail, "no completed second job"},
		{"ask ok", done("submit_rejected", "ask_user"), Observation{SubmitErr: "bad", AskedUser: true}, Pass, ""},
		{"ask before submit", done("submit_rejected", "ask_user"), Observation{AskedUser: true}, Pass, ""},
		{"fix before submit", done("submit_rejected", "fix_and_resubmit"), Observation{JobState: "COMPLETED"}, Pass, ""},
		{"ask missing", done("submit_rejected", "ask_user"), Observation{SubmitErr: "bad"}, Fail, "agent did not ask"},
	}
	for _, tt := range tests {
		v := Judge(tt.c, tt.o)
		if v.Outcome != tt.outcome || v.Reason != tt.reason {
			t.Errorf("%s: got %+v", tt.name, v)
		}
	}
}

func TestClusterHash(t *testing.T) {
	h := ClusterHash("salt", "bigcluster")
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(h) {
		t.Fatalf("bad hash %q", h)
	}
	if h == ClusterHash("salt2", "bigcluster") || h != ClusterHash("salt", "bigcluster") {
		t.Fatal("hash must depend on the salt and be stable")
	}
}

func TestPost(t *testing.T) {
	var got Report
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/api/test-runs" || r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			http.Error(w, "bad", 400)
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	rep := Report{RunRef: "run-12345678", ClientVersion: "dev", ClusterHash: ClusterHash("s", "a"), Scheduler: "slurm",
		StartedAt: time.Now().UTC(), Results: []Result{{Case: "py-numpy-sum", Outcome: Pass, Seconds: 5}}}
	if err := Post(context.Background(), srv.Client(), srv.URL, "tok", rep); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok" || got.RunRef != rep.RunRef || len(got.Results) != 1 {
		t.Fatalf("auth %q got %+v", auth, got)
	}
	if err := Post(context.Background(), srv.Client(), srv.URL+"/x", "tok", rep); err == nil {
		t.Fatal("want error on 404")
	}
}

func TestClassExit127(t *testing.T) {
	if got := Class(Observation{JobState: "FAILED", ExitCode: 127}); got != "missing_software" {
		t.Errorf("got %q", got)
	}
}

func TestSystemID(t *testing.T) {
	yes, no := true, false
	me := func(m account.Me, err error) func(context.Context) (account.Me, error) {
		return func(context.Context) (account.Me, error) { return m, err }
	}
	var asked []account.SystemRef
	res := func(s account.System, err error) func(context.Context, account.SystemRef) (account.System, error) {
		return func(_ context.Context, r account.SystemRef) (account.System, error) {
			asked = append(asked, r)
			return s, err
		}
	}
	ctx := context.Background()
	if got := SystemID(ctx, me(account.Me{Contributes: &yes}, nil), res(account.System{ID: "0b9d4f6e-1c2a-4b7d-9e3f-5a6b7c8d9e0f"}, nil), "bigcluster"); got != "0b9d4f6e-1c2a-4b7d-9e3f-5a6b7c8d9e0f" {
		t.Fatalf("contributing: %q", got)
	}
	if len(asked) != 1 || asked[0].Name != "bigcluster" {
		t.Fatalf("asked %v", asked)
	}
	if got := SystemID(ctx, me(account.Me{}, nil), res(account.System{ID: "0b9d4f6e-1c2a-4b7d-9e3f-5a6b7c8d9e0f"}, nil), "bigcluster"); got != "" || len(asked) != 1 {
		t.Fatalf("contributes unset: %q", got)
	}
	asked = nil
	if got := SystemID(ctx, me(account.Me{Contributes: &no}, nil), res(account.System{ID: "0b9d4f6e-1c2a-4b7d-9e3f-5a6b7c8d9e0f"}, nil), "bigcluster"); got != "" || len(asked) != 0 {
		t.Fatalf("private: %q asked %v", got, asked)
	}
	if got := SystemID(ctx, me(account.Me{}, errors.New("x")), res(account.System{ID: "0b9d4f6e-1c2a-4b7d-9e3f-5a6b7c8d9e0f"}, nil), "c"); got != "" {
		t.Fatalf("me error: %q", got)
	}
	if got := SystemID(ctx, me(account.Me{}, nil), res(account.System{}, errors.New("404")), "c"); got != "" {
		t.Fatalf("resolve error: %q", got)
	}
	b, _ := json.Marshal(Report{})
	if strings.Contains(string(b), "hpc_system_id") {
		t.Fatalf("empty id must be omitted: %s", b)
	}
}

func TestSystemIDBadID(t *testing.T) {
	yes := true
	got := SystemID(context.Background(),
		func(context.Context) (account.Me, error) { return account.Me{Contributes: &yes}, nil },
		func(context.Context, account.SystemRef) (account.System, error) {
			return account.System{ID: "not-a-uuid"}, nil
		}, "c")
	if got != "" {
		t.Fatalf("got %q", got)
	}
}
