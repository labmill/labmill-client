package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"labmill-client/internal/agent"
)

// set_cluster_account (M59.1): add_cluster reuses the account name the user
// already has at an institution; when the sign-in rejects it, the user's real
// name for this cluster is set here. It only changes the User line of the
// cluster's login and files hosts in Labmill's own SSH file; entries of the
// user's own SSH config are never rewritten.

// AccountChange is what set_cluster_account changes, built by
// Env.PlanClusterAccount.
type AccountChange struct {
	// File is Labmill's own SSH file.
	File string
	// Lines are the plain lines shown to the user: one per host, with the
	// old and the new User.
	Lines []string
}

type setAccountInput struct {
	Cluster  string `json:"cluster"`
	Username string `json:"username"`
}

func (t *toolset) setAccountPlan(input json.RawMessage) (setAccountInput, AccountChange, error) {
	var in setAccountInput
	if err := decode(input, &in); err != nil {
		return in, AccountChange{}, err
	}
	in.Cluster, in.Username = strings.TrimSpace(in.Cluster), strings.TrimSpace(in.Username)
	if t.env.PlanClusterAccount == nil || t.env.SetClusterAccount == nil || t.env.Config == nil {
		return in, AccountChange{}, errors.New("Changing the account name isn't available in this front end. The user can change it in Settings or in their SSH settings.")
	}
	if in.Cluster == "" {
		return in, AccountChange{}, errors.New("set_cluster_account needs the cluster's name.")
	}
	if !addClusterUserRE.MatchString(in.Username) {
		return in, AccountChange{}, errors.New("That account name is not valid (lowercase letters, digits, '.', '_' and '-'). Ask the user for their account name at that institution.")
	}
	c, ok := t.env.Config.Cluster(in.Cluster)
	if !ok {
		return in, AccountChange{}, fmt.Errorf("No cluster named %q (list_clusters).", sanitize(in.Cluster))
	}
	ch, err := t.env.PlanClusterAccount(c, in.Username)
	if err != nil {
		return in, AccountChange{}, err
	}
	return in, ch, nil
}

func setAccountProposal(in setAccountInput, ch AccountChange) agent.Proposal {
	var b strings.Builder
	fmt.Fprintf(&b, "Cluster: %s\nNew account name: %s\nFile that changes: %s\n", sanitize(in.Cluster), sanitize(in.Username), sanitize(ch.File))
	for _, l := range ch.Lines {
		fmt.Fprintf(&b, "  %s\n", sanitize(l))
	}
	b.WriteString("Nothing is sent to the cluster now; the next sign-in uses the new name.")
	return agent.Proposal{
		Tool:    "set_cluster_account",
		Summary: fmt.Sprintf("Use the account %s on the cluster %s", sanitize(in.Username), sanitize(in.Cluster)),
		Details: b.String(),
		// Writes SSH settings: always asks, also under "Approve everything
		// automatically" (the same rule as add_cluster).
		Secret: true,
	}
}

func (t *toolset) setClusterAccount() agent.Tool {
	return agent.Tool{
		Name: "set_cluster_account",
		Description: "Change the account (user) name Labmill signs in with on a cluster, for its login and files hosts. Use it only after a sign-in was rejected and the user told you their account name at that institution differs from the one Labmill reused, " +
			"or when the site skill says this cluster uses a different account. It edits only Labmill's own SSH settings and refuses a host that the user's own SSH config defines; the user must approve. " +
			"After a rejected sign-in, do not retry in the same turn: tell the user, and sign in only when they ask again.",
		Schema: objSchema(map[string]any{
			"cluster":  strProp("the cluster's name (list_clusters)"),
			"username": strProp("the user's account name at that cluster's institution, as the user gave it"),
		}, "cluster", "username"),
		NeedsApproval: true,
		Propose: func(input json.RawMessage) (agent.Proposal, error) {
			in, ch, err := t.setAccountPlan(input)
			if err != nil {
				return agent.Proposal{}, err
			}
			return setAccountProposal(in, ch), nil
		},
		Run: func(ctx context.Context, input json.RawMessage, d agent.Decision) (string, error) {
			in, ch, err := t.setAccountPlan(input)
			if err != nil {
				return "", err
			}
			if err := sameAsApproved(setAccountProposal(in, ch), d.Proposal); err != nil {
				return "", err
			}
			c, _ := t.env.Config.Cluster(in.Cluster)
			changed, err := t.env.SetClusterAccount(ctx, c, in.Username)
			if err != nil {
				return "", fmt.Errorf("I couldn't change the account: %w", err)
			}
			return fmt.Sprintf("The account on %s is now %s (SSH hosts changed: %s). The next sign-in uses it; ask the user before signing in again.", in.Cluster, in.Username, strings.Join(changed, ", ")), nil
		},
	}
}
