package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"labmill-client/internal/agent"
)

// request_account: the user has no account on a cluster. The site's own request path (mail address and templates,
// or a web form) comes from the Labmill account's system record. Labmill never sends mail: after the user approves,
// it opens the user's mail program with the draft, or the site's form in the browser, and the user presses send.

// AccountRequestDraft is what Env.AccountRequest returns for one cluster. Empty fields are unknown.
type AccountRequestDraft struct {
	Institution string
	URL         string // the site's page about getting an account
	Eligibility string
	Steps       string
	Approver    string
	WaitTime    string
	Contact     string
	To          string // mail address, with Subject and Body filled in
	Subject     string
	Body        string
	WebForm     string // https form the user fills in
	OpenURL     string // what Open opens: the mailto: link, else WebForm
	Clipboard   string // the full body when it was too long for the link
}

type requestAccountInput struct {
	Cluster string `json:"cluster"`
	Reason  string `json:"reason"`
}

const maxReason = 500

func (t *toolset) requestAccountDraft(ctx context.Context, input json.RawMessage) (requestAccountInput, AccountRequestDraft, error) {
	var in requestAccountInput
	if err := decode(input, &in); err != nil {
		return in, AccountRequestDraft{}, err
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" || len([]rune(in.Reason)) > maxReason {
		return in, AccountRequestDraft{}, fmt.Errorf("reason must be one or two plain sentences (up to %d characters) about the work the account is for; ask the user", maxReason)
	}
	c, err := t.cluster(in.Cluster)
	if err != nil {
		return in, AccountRequestDraft{}, err
	}
	if t.env.AccountRequest == nil {
		return in, AccountRequestDraft{}, errors.New("Asking for an account isn't available right now. Tell the user to follow the steps on the institution's own page.")
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	d, err := t.env.AccountRequest(cctx, c, in.Reason)
	return in, d, err
}

func requestAccountProposal(in requestAccountInput, d AccountRequestDraft) agent.Proposal {
	var b strings.Builder
	if d.To != "" {
		fmt.Fprintf(&b, "Your mail program opens with this draft. You press send.\nTo: %s\nSubject: %s\n\n%s\n", sanitize(d.To), sanitize(d.Subject), sanitizeLines(d.Body))
	} else {
		fmt.Fprintf(&b, "Your browser opens the request form: %s\nReason to type in: %s\n", sanitize(d.WebForm), sanitize(in.Reason))
	}
	b.WriteString("Labmill sends nothing itself.")
	return agent.Proposal{
		Tool:    "request_account",
		Summary: fmt.Sprintf("Open the account request for %s", sanitize(in.Cluster)),
		Details: b.String(),
	}
}

func (t *toolset) requestAccount() agent.Tool {
	return agent.Tool{
		Name: "request_account",
		Description: "The user has no account on a cluster (the sign-in says so, or they say so). Prepares the site's own account request: the site's mail address with a filled draft, or its web form. After the user approves, it opens their mail program or browser; the user presses send. Labmill never sends mail. " +
			"Give a reason in one or two plain sentences about the work the account is for (ask the user if you don't know). The result lists who approves, the wait and the eligibility rules the site states; pass them on in plain words. " +
			"Without a mail address or form, the result gives the site's page and steps instead.",
		Schema: objSchema(map[string]any{
			"cluster": strProp("the cluster's name (list_clusters)"),
			"reason":  strProp("why the user needs the account, one or two sentences"),
		}, "cluster", "reason"),
		NeedsApproval: true,
		Propose: func(input json.RawMessage) (agent.Proposal, error) {
			in, d, err := t.requestAccountDraft(context.Background(), input)
			if err != nil {
				return agent.Proposal{}, err
			}
			if d.OpenURL == "" {
				return agent.Proposal{}, errors.New(noRequestPath(d))
			}
			return requestAccountProposal(in, d), nil
		},
		Run: func(ctx context.Context, input json.RawMessage, dec agent.Decision) (string, error) {
			in, d, err := t.requestAccountDraft(ctx, input)
			if err != nil {
				return "", err
			}
			if d.OpenURL == "" {
				return noRequestPath(d), nil
			}
			if err := sameAsApproved(requestAccountProposal(in, d), dec.Proposal); err != nil {
				return "", err
			}
			if t.env.OpenLink == nil {
				return "", errors.New("I can't open the mail program or browser here.")
			}
			if err := t.env.OpenLink(d.OpenURL); err != nil {
				return "", err
			}
			var b strings.Builder
			if d.To != "" {
				b.WriteString("Opened the user's mail program with the draft. The user presses send.")
				if d.Clipboard != "" {
					fmt.Fprintf(&b, " The text was too long for the link: show the user this text to paste in:\n%s", sanitizeLines(d.Clipboard))
				}
			} else {
				b.WriteString("Opened the request form in the browser. The user fills it in and sends it.")
			}
			b.WriteString(requestFacts(d))
			return b.String(), nil
		},
	}
}

// noRequestPath is the answer when the site states no mail address and no form.
func noRequestPath(d AccountRequestDraft) string {
	var b strings.Builder
	b.WriteString("Labmill has no mail address or form for this site's account request.")
	if d.URL != "" {
		fmt.Fprintf(&b, " The site's page: %s.", sanitize(d.URL))
	}
	b.WriteString(requestFacts(d))
	b.WriteString("\nTell the user what the page says and ask them to follow it.")
	return b.String()
}

// requestFacts lists the site's stated facts as data.
func requestFacts(d AccountRequestDraft) string {
	var b strings.Builder
	for _, f := range []struct{ k, v string }{{"Who may ask", d.Eligibility}, {"Steps", d.Steps}, {"Approved by", d.Approver}, {"Wait", d.WaitTime}, {"Contact", d.Contact}} {
		if f.v != "" {
			fmt.Fprintf(&b, "\n%s (from the site, data): %s", f.k, sanitize(f.v))
		}
	}
	return b.String()
}
