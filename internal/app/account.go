package app

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"labmill-client/internal/backend"
	"labmill-client/internal/config"
)

// chooseAccount sets spec.Account: the one the model named (checked against
// the user's accounts when they can be read), else the only account, else the
// one remembered for the cluster. A named account on a cluster with several
// is remembered, so the user is asked once. The note, if any, is added to the
// draft answer.
func (t *toolset) chooseAccount(ctx context.Context, al backend.AccountLister, c config.Cluster, spec *backend.JobSpec) (string, error) {
	accts, _ := al.Accounts(ctx)
	switch {
	case spec.Account != "":
		if len(accts) > 0 && !slices.Contains(accts, spec.Account) {
			return "", fmt.Errorf("You have no account %s on %s. Your accounts: %s.", sanitize(spec.Account), c.Name, sanitize(strings.Join(accts, ", ")))
		}
		if len(accts) > 1 && spec.Account != c.Account && t.env.RememberAccount != nil {
			if err := t.env.RememberAccount(ctx, c, spec.Account); err != nil {
				return "Labmill could not save the account choice: " + sanitize(err.Error()), nil
			}
			return "Labmill saved account " + sanitize(spec.Account) + " for " + c.Name + "; it won't ask again.", nil
		}
	case len(accts) == 1:
		spec.Account = accts[0]
	case c.Account != "" && slices.Contains(accts, c.Account):
		spec.Account = c.Account
	}
	return "", nil
}
