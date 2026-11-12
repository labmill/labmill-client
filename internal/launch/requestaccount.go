package launch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"labmill-client/internal/account"
	"labmill-client/internal/app"
	"labmill-client/internal/config"
	"labmill-client/internal/mailreq"
	"labmill-client/internal/site"
	"labmill-client/internal/sshconfig"
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
		return buildAccountRequest(sys, me.DisplayName, reason)
	}
}

// buildAccountRequest turns the site's request facts into a draft. A mail needs a valid address; otherwise the web
// form is used; with neither, OpenURL stays empty and the tool reports the site's page.
func buildAccountRequest(sys account.System, name, reason string) (app.AccountRequestDraft, error) {
	r := sys.AccountRequest
	if r == nil {
		return app.AccountRequestDraft{}, errors.New("Labmill knows no account request path for this system yet")
	}
	d := app.AccountRequestDraft{
		Institution: sys.Institution, URL: r.URL, Eligibility: r.Eligibility, Steps: r.Steps,
		Approver: r.Approver, WaitTime: r.WaitTime, Contact: r.Contact, To: r.To, WebForm: r.WebFormURL,
	}
	if name = strings.TrimSpace(name); name == "" {
		name = "(your name)"
	}
	if r.To != "" && r.SubjectTemplate != "" && r.BodyTemplate != "" {
		d.Subject = mailreq.Fill(r.SubjectTemplate, name, sys.Institution, reason)
		d.Body = mailreq.Fill(r.BodyTemplate, name, sys.Institution, reason)
		m, err := mailreq.Mailto(r.To, d.Subject, d.Body)
		if err == nil {
			d.OpenURL, d.Clipboard = m.URL, m.Clipboard
			return d, nil
		}
		d.To, d.Subject, d.Body = "", "", ""
	} else {
		d.To = ""
	}
	if strings.HasPrefix(r.WebFormURL, "https://") {
		d.OpenURL = r.WebFormURL
	}
	return d, nil
}
