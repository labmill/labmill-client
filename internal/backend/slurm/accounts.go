package slurm

import (
	"context"
	"slices"
	"strings"
)

// accountsScript prints the user's associations as Account|DefaultAccount.
// It is read only; a site that hides sacctmgr gives no lines.
const accountsScript = `sacctmgr -nP show assoc user="$USER" format=Account,DefaultAccount 2>/dev/null`

// Accounts returns the Slurm accounts of the user, sorted, and the default
// one ("" when unknown). Any failure gives no accounts: the account is then
// left to the scheduler's default, as before.
func (s *Slurm) Accounts(ctx context.Context) (accounts []string, def string) {
	res, err := s.Runner.Run(ctx, []string{"sh", "-c", accountsScript}, nil)
	if err != nil || res.ExitCode != 0 {
		return nil, ""
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		a := strings.TrimSpace(f[0])
		if a == "" || !validAccount(a) {
			continue
		}
		if !slices.Contains(accounts, a) {
			accounts = append(accounts, a)
		}
		if len(f) > 1 && def == "" && validAccount(strings.TrimSpace(f[1])) {
			def = strings.TrimSpace(f[1])
		}
	}
	slices.Sort(accounts)
	if def != "" && !slices.Contains(accounts, def) {
		def = ""
	}
	return accounts, def
}

// validAccount matches the token characters JobSpec.Validate accepts.
func validAccount(a string) bool {
	if a == "" {
		return false
	}
	for _, r := range a {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
