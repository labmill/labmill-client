package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/account"
	"labmill-client/internal/app"
	"labmill-client/internal/config"
	"labmill-client/internal/store"
)

func helpEnv(t *testing.T, contributes bool) (*app.Env, int64, *string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))
	var posted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/me":
			json.NewEncoder(w).Encode(map[string]any{"person_id": "p", "contributes": contributes,
				"tenants": []account.Tenant{{ID: "t1", Name: "Example RCI"}}})
		case "POST /api/help":
			b, _ := io.ReadAll(r.Body)
			posted = string(b)
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"id":"h1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	p, err := account.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := account.Save(p, account.Link{URL: srv.URL, Token: "lmt_abcdefghijklmnopqrstuvwxyz012345", LinkedAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	id, _ := db.CreateJob(ctx, store.Job{Name: "train", Cluster: "bigcluster", Backend: "slurm", Script: "#!/bin/bash\npython train.py\n"})
	db.UpdateJob(ctx, id, func(j *store.Job) error { j.State, j.RemoteID = store.StateFailed, "4242"; return nil })
	env := &app.Env{}
	wireHelp(env, db, &config.Config{}, nil)
	return env, id, &posted
}

func TestRSEHelpDetailsOnlyForContributingAccount(t *testing.T) {
	for _, contributes := range []bool{true, false} {
		env, id, posted := helpEnv(t, contributes)
		h, err := env.RSEPrepare(context.Background(), id, "", true)
		if err != nil || h.Tenant != "t1" || h.Team != "Example RCI" {
			t.Fatalf("prepare: %+v %v", h, err)
		}
		if got := strings.Contains(h.Report, `"script"`); got != contributes {
			t.Errorf("contributes=%v: script in report = %v: %s", contributes, got, h.Report)
		}
		if _, err := env.RSESend(context.Background(), h, "it fails"); err != nil {
			t.Fatal(err)
		}
		var in account.HelpIn
		json.Unmarshal([]byte(*posted), &in)
		if in.Tenant != "t1" || in.Body != "it fails" || !sameJSON(in.Report, h.Report) {
			t.Errorf("sent %s, approved %s", *posted, h.Report)
		}
	}
}

func sameJSON(a json.RawMessage, b string) bool {
	var x, y bytes.Buffer
	return json.Compact(&x, a) == nil && json.Compact(&y, []byte(b)) == nil && x.String() == y.String()
}
