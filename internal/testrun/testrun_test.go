package testrun

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

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
