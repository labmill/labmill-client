package failclass

import "testing"

func TestOf(t *testing.T) {
	tests := []struct {
		name string
		in   Input
		want string
	}{
		{"completed", Input{State: "COMPLETED"}, ""},
		{"completed with noise", Input{State: "COMPLETED", Output: "No such file or directory"}, ""},
		{"submit error", Input{SubmitErr: "invalid account"}, SubmitRejected},
		{"timeout", Input{State: "TIMEOUT", ExitCode: 1}, Timeout},
		{"slurm oom state", Input{State: "OUT_OF_MEMORY"}, OutOfMemory},
		{"oom text", Input{State: "FAILED", ExitCode: 137, Output: "slurmstepd: error: Detected 1 oom-kill event"}, OutOfMemory},
		{"python memory error", Input{State: "FAILED", ExitCode: 1, Output: "MemoryError"}, OutOfMemory},
		{"exit 127", Input{State: "FAILED", ExitCode: 127}, MissingSoftware},
		{"command not found", Input{State: "FAILED", ExitCode: 1, Output: "run.sh: line 3: nosuchtool: command not found"}, MissingSoftware},
		{"python module", Input{State: "FAILED", ExitCode: 1, Output: "ModuleNotFoundError: No module named 'numpy'"}, MissingSoftware},
		{"missing file", Input{State: "FAILED", ExitCode: 2, Output: "cannot open in.csv: No such file or directory"}, DataMissing},
		{"exit 1", Input{State: "FAILED", ExitCode: 1}, ExitNonzero},
		{"failed, exit unknown", Input{State: "FAILED", ExitCode: -1}, ExitNonzero},
		{"failed with exit 0 (signal)", Input{State: "FAILED"}, ""},
		{"env interpreter", Input{State: "FAILED", ExitCode: 127, Output: "/usr/bin/env: 'python3': No such file or directory"}, MissingSoftware},
		{"env interpreter, other exit", Input{State: "FAILED", ExitCode: 1, Output: "/usr/bin/env: 'python3': No such file or directory"}, MissingSoftware},
		{"cancelled", Input{State: "CANCELLED"}, ""},
		{"node fail with exit", Input{State: "NODE_FAIL", ExitCode: 1}, ExitNonzero},
		{"empty", Input{}, ""},
	}
	for _, tt := range tests {
		if got := Of(tt.in); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}
