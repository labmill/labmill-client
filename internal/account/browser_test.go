package account

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The browser flow end to end against a fake account: the code comes back on the loopback callback, the verifier
// matches the challenge, and the token is saved.
func TestBrowserLogin(t *testing.T) {
	const tok = "lmt_browserbrowserbrowserbrowser"
	var challenge string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/client-auth/token":
			var in struct{ Code, Verifier string }
			json.NewDecoder(r.Body).Decode(&in)
			sum := sha256.Sum256([]byte(in.Verifier))
			if in.Code != "code1" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				http.Error(w, "bad code", 400)
				return
			}
			w.Write([]byte(`{"token":"` + tok + `"}`))
		case "/api/me":
			if r.Header.Get("Authorization") != "Bearer "+tok {
				http.Error(w, "sign in first", 401)
				return
			}
			w.Write([]byte(`{"person_id":"p1","role":"user","display_name":"B User"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	path := filepath.Join(t.TempDir(), "account.json")
	browser := func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil || u.Path != "/connect" || u.Query().Get("name") != "pc1" {
			t.Errorf("connect URL %q", raw)
		}
		q := u.Query()
		challenge = q.Get("challenge")
		go func() {
			// A wrong state is refused; the right one delivers the code.
			cb := "http://127.0.0.1:" + q.Get("port") + "/callback?"
			if r, err := http.Get(cb + url.Values{"state": {"wrong"}, "code": {"x"}}.Encode()); err != nil || r.StatusCode != 400 {
				t.Errorf("wrong state: %v %v", r, err)
			}
			r, err := http.Get(cb + url.Values{"state": {q.Get("state")}, "code": {"code1"}}.Encode())
			if err != nil || r.StatusCode != 200 {
				t.Errorf("callback: %v %v", r, err)
			}
		}()
		return nil
	}
	me, err := BrowserLogin(context.Background(), path, api.URL, "pc1", browser, nil, time.Now())
	if err != nil || me.DisplayName != "B User" {
		t.Fatalf("login: %+v %v", me, err)
	}
	if l, ok, err := Load(path); !ok || err != nil || l.Token != tok {
		t.Fatalf("saved link: %+v %v %v", l, ok, err)
	}
	// A cancelled wait returns at once and saves nothing new.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := BrowserLogin(ctx, filepath.Join(t.TempDir(), "a.json"), api.URL, "", func(string) error { return nil }, nil, time.Now()); err == nil || strings.Contains(err.Error(), "10 minutes") {
		t.Fatalf("cancelled: %v", err)
	}
}

// A refused sign-in says what to do, never raw server text or a token message when no token exists yet.
func TestBrowserLoginRefusals(t *testing.T) {
	var status atomic.Int32
	status.Store(400)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer api.Close()
	run := func(cb url.Values) error {
		browser := func(raw string) error {
			u, _ := url.Parse(raw)
			q := u.Query()
			cb.Set("state", q.Get("state"))
			go func() {
				if r, err := http.Get("http://127.0.0.1:" + q.Get("port") + "/callback?" + cb.Encode()); err == nil {
					r.Body.Close()
				}
			}()
			return nil
		}
		_, err := BrowserLogin(context.Background(), filepath.Join(t.TempDir(), "a.json"), api.URL, "", browser, nil, time.Now())
		return err
	}
	for _, c := range []struct {
		code int
		want string
	}{{400, "already used or has expired"}, {401, "already used or has expired"}, {410, "already used or has expired"}, {429, "too many sign-in tries"}} {
		status.Store(int32(c.code))
		if err := run(url.Values{"code": {"c1"}}); err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "program token") {
			t.Errorf("status %d: %v", c.code, err)
		}
	}
	if err := run(url.Values{"error": {"access_denied"}}); err == nil || !strings.Contains(err.Error(), "access_denied") || !strings.Contains(err.Error(), "labmill login") {
		t.Errorf("denied in the browser: %v", err)
	}
}

func TestStatusErrorText(t *testing.T) {
	for _, c := range []struct {
		code int
		text string
		want string
	}{
		{409, `{"error":"you have 10 program tokens; revoke one in Settings"}`, "the Labmill account refused this (409): you have 10 program tokens; revoke one in Settings"},
		{409, "too many program tokens (10): revoke one in Settings first", "refused this (409): too many program tokens (10): revoke one in Settings first"},
		{409, "bad\x1b[31m red", "(409): bad red"},
		{422, `{"detail":[1]}`, "the Labmill account refused this (422)"},
		{404, "<html>Not found</html>", "the Labmill account refused this (404)"},
		{502, "<html>Bad gateway</html>", "has a problem (502)"},
		{401, "x", "did not accept the token"},
	} {
		if got := (&StatusError{Code: c.code, Text: c.text}).Error(); !strings.Contains(got, c.want) {
			t.Errorf("%d %q: %q", c.code, c.text, got)
		}
	}
}

func TestBrowserLoginRefusedValueIsShort(t *testing.T) {
	long := "bad\x1b[31m" + strings.Repeat("x", 500) + "\nline"
	browser := func(raw string) error {
		u, _ := url.Parse(raw)
		q := u.Query()
		go func() {
			if r, err := http.Get("http://127.0.0.1:" + q.Get("port") + "/callback?" + url.Values{"state": {q.Get("state")}, "error": {long}}.Encode()); err == nil {
				r.Body.Close()
			}
		}()
		return nil
	}
	_, err := BrowserLogin(context.Background(), filepath.Join(t.TempDir(), "a.json"), "http://127.0.0.1:1", "", browser, nil, time.Now())
	if err == nil || strings.ContainsAny(err.Error(), "\x1b\n") || len(err.Error()) > 200 {
		t.Fatalf("long error value: %q", err)
	}
}

func TestStatusErrorOtherCodes(t *testing.T) {
	if got := (&StatusError{Code: 404, Text: "no\nsuch\x1b[1m page"}).Error(); got != "the Labmill account refused this (404): no such page" {
		t.Errorf("%q", got)
	}
}

func TestWhoamiCleansNames(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"person_id":"` + strings.Repeat("p", 300) + `","display_name":"Ann\u001b[31m\nLee"}`))
	}))
	defer api.Close()
	me, err := (&Client{URL: api.URL, Token: "lmt_x"}).Whoami(context.Background())
	if err != nil || me.DisplayName != "Ann Lee" || len([]rune(me.PersonID)) != 103 || !strings.HasSuffix(me.PersonID, "...") {
		t.Fatalf("%+v %v", me, err)
	}
}

func TestBrowserLogin400ShowsReason(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"verifier does not match"}`, 400)
	}))
	defer api.Close()
	browser := func(raw string) error {
		u, _ := url.Parse(raw)
		q := u.Query()
		go func() {
			if r, err := http.Get("http://127.0.0.1:" + q.Get("port") + "/callback?" + url.Values{"state": {q.Get("state")}, "code": {"c"}}.Encode()); err == nil {
				r.Body.Close()
			}
		}()
		return nil
	}
	_, err := BrowserLogin(context.Background(), filepath.Join(t.TempDir(), "a.json"), api.URL, "", browser, nil, time.Now())
	if err == nil || !strings.Contains(err.Error(), "verifier does not match") {
		t.Fatalf("%v", err)
	}
}

func TestDetailKeepsServerText(t *testing.T) {
	if got := Detail(&StatusError{Code: 502, Text: "upstream down"}); !strings.Contains(got, "upstream down") {
		t.Errorf("%q", got)
	}
	if got := Detail(&StatusError{Code: 502, Text: "<html>Bad gateway</html>"}); strings.Contains(got, "html") {
		t.Errorf("%q", got)
	}
	if got := Detail(&StatusError{Code: 409, Text: "x"}); got != (&StatusError{Code: 409, Text: "x"}).Error() {
		t.Errorf("%q", got)
	}
}
