package launch

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"labmill-client/internal/account"
	"labmill-client/internal/app"
	"labmill-client/internal/config"
	"labmill-client/internal/mailreq"
	"labmill-client/internal/site"
	"labmill-client/internal/sshconfig"
	"labmill-client/internal/textsafe"
)

// systemRef is what the client knows of cluster c for GET /api/systems/resolve.
func systemRef(st site.Site, others []site.Site, c config.Cluster) account.SystemRef {
	ref := account.SystemRef{Name: c.Name, Domain: clusterSite(st, c, others).Domain}
	if paths, err := sshconfig.DefaultPaths(); err == nil && c.SSHHost != "" {
		if h, _, err := sshconfig.Lookup(paths, c.SSHHost); err == nil {
			ref.Host = h.HostName
		}
	}
	return ref
}

// wireRequestAccount sets Env.AccountRequest and Env.OpenLink (request_account).
func wireRequestAccount(env *app.Env, st site.Site, others []site.Site) {
	env.OpenLink = mailreq.Open
	env.AccountRequest = func(ctx context.Context, c config.Cluster, reason string) (app.AccountRequestDraft, error) {
		path, err := account.Path()
		if err != nil {
			return app.AccountRequestDraft{}, err
		}
		l, ok, err := account.Load(path)
		if err != nil {
			return app.AccountRequestDraft{}, err
		}
		if !ok {
			return app.AccountRequestDraft{}, account.ErrNotLinked
		}
		cl := &account.Client{URL: l.URL, Token: l.Token}
		me, err := cl.Whoami(ctx)
		if err != nil {
			return app.AccountRequestDraft{}, err
		}
		sys, err := cl.Resolve(ctx, systemRef(st, others, c))
		if err != nil {
			return app.AccountRequestDraft{}, fmt.Errorf("I couldn't find %s in the Labmill list: %w", c.Name, err)
		}
		return buildAccountRequest(sys, me.DisplayName, reason, clusterSite(st, c, others).Domain)
	}
}

// Neutral texts for a site that states an address but no template. Same placeholders as a site template.
const (
	defaultSubject = "Account request: {name}"
	defaultBody    = "Hello,\n\nI am {name} at {institution}. I would like to ask for an account on your cluster.\n\n{reason}\n\nThank you."
)

// buildAccountRequest turns the site's request facts into a draft. A mail needs a valid address (without a stated
// template a neutral one is used); otherwise the web form is used; with neither, OpenURL stays empty and the tool
// reports the site's page. Every text is cleaned once, and the card shows what opens. domain is the institution's
// domain: an address or form host outside it gets a warning on the card (not a block).
func buildAccountRequest(sys account.System, name, reason, domain string) (app.AccountRequestDraft, error) {
	r := sys.AccountRequest
	if r == nil {
		return app.AccountRequestDraft{}, errors.New("Labmill knows no account request path for this system yet")
	}
	clean := func(s string) string { return strings.TrimSpace(textsafe.Clean(s)) }
	inst := mailreq.OneLine(sys.Institution)
	d := app.AccountRequestDraft{
		Institution: inst, URL: clean(r.URL), Eligibility: clean(r.Eligibility), Steps: clean(r.Steps),
		Approver: clean(r.Approver), WaitTime: clean(r.WaitTime), Contact: clean(r.Contact), WebForm: clean(r.WebFormURL),
	}
	if name = mailreq.OneLine(name); name == "" {
		name = "(your name)"
	}
	reason = clean(reason)
	if to := clean(r.To); to != "" {
		subj, body := r.SubjectTemplate, r.BodyTemplate
		if strings.TrimSpace(subj) == "" {
			subj = defaultSubject
		}
		if strings.TrimSpace(body) == "" {
			body = defaultBody
		}
		subject := mailreq.OneLine(mailreq.Fill(subj, name, inst, reason))
		text := clean(mailreq.Fill(body, name, inst, reason))
		if m, err := mailreq.Mailto(to, subject, text); err == nil {
			d.To, d.Subject, d.Body, d.OpenURL, d.Clipboard = to, subject, text, m.URL, m.Clipboard
			if m.Clipboard != "" {
				d.Body = mailreq.LongBodyNote
			}
			d.Warning = foreignWarning("The mail address", strings.TrimPrefix(to[strings.LastIndex(to, "@"):], "@"), domain)
			return d, nil
		}
	}
	if u, err := url.Parse(d.WebForm); err == nil && u.Scheme == "https" && u.Host != "" {
		d.OpenURL = d.WebForm
		d.Warning = foreignWarning("The form", u.Hostname(), domain)
	} else {
		d.WebForm = ""
	}
	return d, nil
}

// foreignWarning is a warning when host is not domain or a subdomain of it; "" when it is, or domain is unknown.
func foreignWarning(what, host, domain string) string {
	host, domain = strings.ToLower(host), strings.ToLower(domain)
	if !strings.Contains(domain, ".") || host == domain || strings.HasSuffix(host, "."+domain) {
		return ""
	}
	return fmt.Sprintf("%s is at %s, not at %s. Check that this is the right place before you send anything.", what, host, domain)
}
