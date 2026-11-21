package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// BrowserTimeout is how long BrowserLogin waits for the browser.
const BrowserTimeout = 10 * time.Minute

// BrowserLogin signs in through the browser (owner, 2026-11-12): it listens on 127.0.0.1, calls open with the
// account's /connect page, waits for the one-time code on /callback, trades it with the PKCE verifier for a program
// token (POST /api/client-auth/token), checks the token and saves the link at path. name labels the token
// ("Labmill client on <name>"). The token never appears in a URL.
func BrowserLogin(ctx context.Context, path, baseURL, name string, open func(url string) error, hc *http.Client, now time.Time) (Me, error) {
	if err := CheckURL(baseURL); err != nil {
		return Me{}, err
	}
	state, err := random(16)
	if err != nil {
		return Me{}, err
	}
	verifier, err := random(32)
	if err != nil {
		return Me{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Me{}, fmt.Errorf("can't wait for the browser: %w", err)
	}
	codes := make(chan string, 1)
	refused := make(chan string, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path == "/callback" && q.Get("state") == state && q.Get("error") != "" {
			http.Error(w, "The sign-in was not finished. Go back to the Labmill program and start it again.", http.StatusBadRequest)
			select {
			case refused <- q.Get("error"):
			default:
			}
			return
		}
		if r.URL.Path != "/callback" || q.Get("state") != state || q.Get("code") == "" {
			http.Error(w, "This is not the sign-in Labmill is waiting for. Start it again from the Labmill program.", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>Labmill</title><p style="font:16px sans-serif;margin:3em">Labmill got the sign-in. Go back to the Labmill program to see the result.</p>`)
		select {
		case codes <- q.Get("code"):
		default:
		}
	})}
	go srv.Serve(ln) //nolint:errcheck // ends with Close
	defer srv.Close()
	q := url.Values{"port": {fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)}, "state": {state},
		"challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}}
	if name != "" {
		q.Set("name", name)
	}
	if err := open(baseURL + "/connect?" + q.Encode()); err != nil {
		return Me{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, BrowserTimeout)
	defer cancel()
	var code string
	select {
	case code = <-codes:
	case why := <-refused:
		return Me{}, fmt.Errorf("the sign-in in the browser was not completed (%s): run labmill login to try again", oneLine(why, 64))
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Me{}, errors.New("the sign-in in the browser did not finish in 10 minutes: run labmill login to try again")
		}
		return Me{}, ctx.Err()
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := (&Client{URL: baseURL, HTTP: hc}).do(ctx, http.MethodPost, "/api/client-auth/token",
		map[string]string{"code": code, "verifier": verifier}, &out); err != nil {
		var se *StatusError
		switch {
		case errors.As(err, &se) && se.Code == http.StatusTooManyRequests:
			return Me{}, errors.New("too many sign-in tries: wait a minute, then run labmill login again")
		case errors.As(err, &se) && se.Code == 400 && plainText(se.Text) != "":
			// Maybe a used code, maybe a request fault: show the server's reason.
			return Me{}, fmt.Errorf("the sign-in code was refused (%s): run labmill login again", plainText(se.Text))
		case errors.As(err, &se) && (se.Code == 400 || se.Code == 401 || se.Code == 403 || se.Code == 410):
			// The code is one-time and short-lived; no token was involved yet.
			return Me{}, errors.New("the sign-in code was already used or has expired: run labmill login again")
		}
		return Me{}, err
	}
	return Login(ctx, path, baseURL, out.Token, hc, now)
}

func random(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
