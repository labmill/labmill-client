package account

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/store"
)

func workedEvents(t *testing.T, f *fakeAPI) []ClientEvent {
	t.Helper()
	var out []ClientEvent
	for _, raw := range f.events {
		var body struct{ Events []ClientEvent }
		json.Unmarshal([]byte(raw), &body)
		out = append(out, body.Events...)
	}
	return out
}

func TestWorkedEventIsSentWithTheJobRef(t *testing.T) {
	for _, private := range []bool{false, true} {
		f := &fakeAPI{contributes: &yes}
		srv := httptest.NewServer(f.handler(t))
		st := newStore(t)
		p := link(t, srv.URL, time.Now().Add(-time.Minute))
		id := addJob(t, st, store.StateCompleted, "")
		if private {
			st.AddEvent(context.Background(), id, EventPrivate, "")
		}
		pr := &Problems{Store: st, LinkPath: p, Version: "1.0", OS: "linux"}
		pr.Worked(id, true)
		if _, err := uploader(st, p).Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		evs := workedEvents(t, f)
		if len(evs) != 1 || evs[0].Kind != KindWorked || evs[0].Message != "yes" || evs[0].ClientJobRef == "" {
			t.Errorf("private=%v: events = %+v", private, evs)
		}
		srv.Close()
	}
}

func TestWorkedEventWithoutRefOrFullPrivacyIsDropped(t *testing.T) {
	f := &fakeAPI{contributes: &no}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	st := newStore(t)
	p := link(t, srv.URL, time.Now().Add(-time.Minute))
	id := addJob(t, st, store.StateCompleted, "")
	(&Problems{Store: st, LinkPath: p}).Worked(id, false)
	uploader(st, p).Pass(context.Background())
	if len(f.events) != 0 {
		t.Errorf("a fully private account sent %v", f.events)
	}
	// Not linked: nothing queued.
	(&Problems{Store: st, LinkPath: t.TempDir() + "/none.json"}).Worked(id, true)
	if due, _ := st.DueClientEvents(context.Background(), time.Now().Add(time.Hour), 10); len(due) != 0 {
		t.Error("queued while logged out")
	}
}

func TestTenantsAndHelp(t *testing.T) {
	var got HelpIn
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/tenants":
			io.WriteString(w, `[{"id":"t1","name":"Example RCI"}]`)
		case "POST /api/help":
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &got)
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"id":"h1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL, Token: goodToken}
	ts, err := c.Tenants(context.Background())
	if err != nil || len(ts) != 1 || ts[0].ID != "t1" {
		t.Fatalf("tenants = %v, %v", ts, err)
	}
	id, err := c.Help(context.Background(), HelpIn{Tenant: "t1", Body: "my job fails", Report: json.RawMessage(`{"a":1}`)})
	if err != nil || id != "h1" || got.Tenant != "t1" || got.Body != "my job fails" || string(got.Report) != `{"a":1}` {
		t.Errorf("help = %q, %v, %+v", id, err, got)
	}
}

func TestPickTenant(t *testing.T) {
	one := []Tenant{{ID: "a", Name: "Alpha"}}
	two := append(one, Tenant{ID: "b", Name: "Beta"})
	if got, err := PickTenant(one, ""); err != nil || got.ID != "a" {
		t.Errorf("one: %v %v", got, err)
	}
	if got, err := PickTenant(two, "beta"); err != nil || got.ID != "b" {
		t.Errorf("by name: %v %v", got, err)
	}
	if _, err := PickTenant(two, ""); err == nil || !strings.Contains(err.Error(), "Alpha, Beta") {
		t.Errorf("open choice: %v", err)
	}
	if _, err := PickTenant(nil, ""); err == nil {
		t.Error("no tenants: no error")
	}
}

func TestHelpReportFitsTheLimit(t *testing.T) {
	st := newStore(t)
	id := addJob(t, st, store.StateFailed, "")
	j, _ := st.Job(context.Background(), id)
	evs, _ := st.Events(context.Background(), id, 0)
	b, err := HelpReport(j, evs, "ref1", NewRedactor(), true)
	if err != nil || len(b) > MaxHelpReport || !json.Valid(b) {
		t.Fatalf("report: %d bytes, %v", len(b), err)
	}
}
