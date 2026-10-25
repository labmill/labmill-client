package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoginWhoamiLogoutCommands(t *testing.T) {
	const tok = "lmt_clitestclitestclitestclitest"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/me" || r.Header.Get("Authorization") != "Bearer "+tok {
			http.Error(w, "sign in first", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"person_id":"p1","role":"user","display_name":"Test User"}`))
	}))
	defer srv.Close()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("LABMILL_ACCOUNT_URL", srv.URL)
	ctx := context.Background()
	call := func(stdin string, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := run(ctx, args, strings.NewReader(stdin), &out, &errb)
		return code, out.String(), errb.String()
	}
	if code, out, _ := call("", "whoami"); code != 1 || !strings.Contains(out, "Not connected") {
		t.Fatalf("whoami before login: %d %q", code, out)
	}
	if code, _, errs := call("lmt_wrongwrongwrongwrongwrong\n", "login"); code != 1 || !strings.Contains(errs, "did not accept") {
		t.Fatalf("bad login: %d %q", code, errs)
	}
	code, out, errs := call(tok+"\n", "login")
	if code != 0 || !strings.Contains(out, "Test User") {
		t.Fatalf("login: %d %q %q", code, out, errs)
	}
	if strings.Contains(out+errs, tok) {
		t.Error("the token was printed")
	}
	if code, out, _ := call("", "whoami"); code != 0 || !strings.Contains(out, "Test User") || strings.Contains(out, tok) {
		t.Fatalf("whoami: %d %q", code, out)
	}
	if code, _, _ := call("", "logout"); code != 0 {
		t.Fatal("logout failed")
	}
	if code, _, _ := call("", "whoami"); code != 1 {
		t.Fatal("still connected after logout")
	}
}
