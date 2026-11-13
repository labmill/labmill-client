package launch

import (
	"context"
	"net/http"
	"time"

	"labmill-client/internal/account"
	"labmill-client/internal/app"
	"labmill-client/internal/config"
	"labmill-client/internal/store"
)

// wireHelp sets the tools that talk to the Labmill account: job_worked
// (queues the "worked" event) and ask_rse (POST /api/help). Both stay nil
// without an account path.
func wireHelp(env *app.Env, db *store.Store, cfg *config.Config, probs *problemTracker) {
	path, err := account.Path()
	if err != nil || db == nil {
		return
	}
	if probs != nil {
		env.Worked = func(_ context.Context, jobID int64, yes bool) error {
			probs.p.Worked(jobID, yes)
			return nil
		}
	}
	client := func() (*account.Client, error) {
		l, ok, err := account.Load(path)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, account.ErrNotLinked
		}
		return &account.Client{URL: l.URL, Token: l.Token, HTTP: &http.Client{Timeout: 20 * time.Second}}, nil
	}
	env.RSEPrepare = func(ctx context.Context, jobID int64, team string, details bool) (app.RSEHelp, error) {
		c, err := client()
		if err != nil {
			return app.RSEHelp{}, err
		}
		me, err := c.Whoami(ctx) // the teams and whether the account contributes
		if err != nil {
			return app.RSEHelp{}, err
		}
		t, err := account.PickTenant(me.Tenants, team)
		if err != nil {
			return app.RSEHelp{}, err
		}
		h := app.RSEHelp{Tenant: t.ID, Team: t.Name}
		if jobID == 0 {
			return h, nil
		}
		j, err := db.Job(ctx, jobID)
		if err != nil {
			return h, err
		}
		evs, err := db.Events(ctx, jobID, 0)
		if err != nil {
			return h, err
		}
		ref := ""
		if up, ok, _ := db.UploadOf(ctx, j.ID, j.RemoteID); ok {
			ref = up.Ref
		}
		// A private job or a fully private account sends scheduler facts only.
		full := details && !account.Private(evs) && me.Contributes != nil && *me.Contributes
		red := account.NewRedactor(append(reportWords(cfg, j), j.Name)...)
		rep, err := account.HelpReport(j, evs, ref, red, full)
		h.Report = string(rep)
		return h, err
	}
	env.RSESend = func(ctx context.Context, h app.RSEHelp, body string) (string, error) {
		c, err := client()
		if err != nil {
			return "", err
		}
		in := account.HelpIn{Tenant: h.Tenant, Body: body}
		if h.Report != "" {
			in.Report = []byte(h.Report)
		}
		return c.Help(ctx, in)
	}
}
