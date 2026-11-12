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
