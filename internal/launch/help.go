package launch

import (
	"context"
	"errors"
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
	prepare := func(ctx context.Context, jobID int64, team string, details bool) (*account.Client, account.Tenant, []byte, error) {
		c, err := client()
		if err != nil {
			return nil, account.Tenant{}, nil, err
		}
		ts, err := c.Tenants(ctx)
		if err != nil {
			return nil, account.Tenant{}, nil, err
		}
		t, err := account.PickTenant(ts, team)
		if err != nil {
			return nil, account.Tenant{}, nil, err
		}
		if jobID == 0 {
			return c, t, nil, nil
		}
		j, err := db.Job(ctx, jobID)
		if err != nil {
			return nil, account.Tenant{}, nil, err
		}
		evs, err := db.Events(ctx, jobID, 0)
		if err != nil {
			return nil, account.Tenant{}, nil, err
		}
		ref := ""
		if up, ok, _ := db.UploadOf(ctx, j.ID, j.RemoteID); ok {
			ref = up.Ref
		}
		red := account.NewRedactor(append(reportWords(cfg, j), j.Name)...)
		rep, err := account.HelpReport(j, evs, ref, red, details && !account.Private(evs))
		if err != nil {
			return nil, account.Tenant{}, nil, err
		}
		return c, t, rep, nil
	}
	env.RSEPrepare = func(ctx context.Context, jobID int64, team string, details bool) (app.RSEHelp, error) {
		_, t, rep, err := prepare(ctx, jobID, team, details)
		return app.RSEHelp{Team: t.Name, Report: string(rep)}, err
	}
	env.RSESend = func(ctx context.Context, jobID int64, team, body string, details bool) (string, error) {
		c, t, rep, err := prepare(ctx, jobID, team, details)
		if err != nil {
			return "", err
		}
		if t.ID == "" {
			return "", errors.New("no team")
		}
		return c.Help(ctx, account.HelpIn{Tenant: t.ID, Body: body, Report: rep})
	}
}
