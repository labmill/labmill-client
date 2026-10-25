// Package account links the client to the user's Labmill account
// (app.labmill.org) with a program token, and sends the records of finished
// jobs to it (upload.go). Nothing is sent while no token is saved.
//
// The token is a secret. It is kept in account.json next to clusters.yaml,
// mode 0600, written atomically (the client keeps no other secret on disk and
// uses no OS key store). It is never logged, shown, or sent anywhere but the
// account's API in the Authorization header.
package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"labmill-client/internal/config"
)

// DefaultURL is the Labmill web app; LABMILL_ACCOUNT_URL overrides it (tests, staging).
const DefaultURL = "https://app.labmill.org"

// tokenPrefix starts every program token (Settings > Program tokens in the web app).
const tokenPrefix = "lmt_"

// Link is the saved account link.
type Link struct {
	URL      string    `json:"url"`
	Token    string    `json:"token"`
	LinkedAt time.Time `json:"linked_at"` // jobs that end before this are not sent
}

// Me is the answer of GET /api/me.
type Me struct {
	PersonID    string `json:"person_id"`
	Role        string `json:"role"`
	DisplayName string `json:"display_name"`
}

// ErrNotLinked means no token is saved.
var ErrNotLinked = errors.New("this computer is not connected to a Labmill account (run: labmill login)")

// ErrRejected means the API refused the token (revoked, expired or wrong).
var ErrRejected = errors.New("the Labmill account did not accept the token: make a new program token and connect again")

// Path is account.json next to clusters.yaml.
func Path() (string, error) {
	cfg, err := config.DefaultPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(cfg), "account.json"), nil
}

// BaseURL is LABMILL_ACCOUNT_URL, else DefaultURL, checked by CheckURL.
func BaseURL() (string, error) {
	u := strings.TrimRight(strings.TrimSpace(os.Getenv("LABMILL_ACCOUNT_URL")), "/")
	if u == "" {
		u = DefaultURL
	}
	return u, CheckURL(u)
}

// CheckURL accepts an absolute https URL, or plain http for a loopback host
// only (as the broker does), so the token never travels in clear text.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("account URL %q must be an absolute https URL", raw)
	}
	if u.Scheme == "http" {
		h := u.Hostname()
		ip := net.ParseIP(h)
		if h != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("account URL %q uses plain http; https is required except for loopback hosts", raw)
		}
	}
	return nil
}

// CheckToken checks the form of a pasted token (not whether it is valid).
func CheckToken(tok string) error {
	if !strings.HasPrefix(tok, tokenPrefix) || len(tok) >= 100 || len(tok) < len(tokenPrefix)+16 {
		return errors.New("that is not a Labmill program token (it starts with lmt_); copy it again from Settings > Program tokens at app.labmill.org")
	}
	for _, r := range tok {
		if r <= ' ' || r > '~' {
			return errors.New("the token has spaces or other characters a token can't have; copy it again")
		}
	}
	return nil
}

// Load reads the link at path. ok is false when there is none.
func Load(path string) (l Link, ok bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Link{}, false, nil
	}
	if err != nil {
		return Link{}, false, err
	}
	if err := json.Unmarshal(b, &l); err != nil || CheckToken(l.Token) != nil || CheckURL(l.URL) != nil {
		return Link{}, false, fmt.Errorf("%s is damaged; run labmill logout and labmill login again", path)
	}
	return l, true, nil
}

// Save writes l to path (atomic, mode 0600, folder 0700).
func Save(path string, l Link) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return config.WriteFile(path, b)
}

// Remove deletes the link (logout). No link is not an error.
func Remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Client calls the account's API with the token.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client // nil: a client with a 30 s timeout
}

// StatusError is an answer other than success. Code 0 means no answer (network).
type StatusError struct {
	Code int
	Text string
}

func (e *StatusError) Error() string {
	if e.Code == http.StatusUnauthorized || e.Code == http.StatusForbidden {
		return ErrRejected.Error()
	}
	return fmt.Sprintf("the Labmill account answered %d: %s", e.Code, e.Text)
}

// Is makes errors.Is(err, ErrRejected) true for 401 and 403.
func (e *StatusError) Is(target error) bool {
	return target == ErrRejected && (e.Code == http.StatusUnauthorized || e.Code == http.StatusForbidden)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	// No redirects: a redirect could carry the token to another host.
	cl := *hc
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := cl.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL is not secret, but keep the message short
		}
		return fmt.Errorf("can't reach the Labmill account: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return &StatusError{Code: resp.StatusCode, Text: strings.TrimSpace(string(b))}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// Whoami calls GET /api/me.
func (c *Client) Whoami(ctx context.Context) (Me, error) {
	var me Me
	err := c.do(ctx, http.MethodGet, "/api/me", nil, &me)
	return me, err
}

// System is one entry of GET /api/systems.
type System struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Institution string `json:"institution"`
}

// Systems calls GET /api/systems?q= (a public route; the token is sent anyway).
func (c *Client) Systems(ctx context.Context, q string) ([]System, error) {
	var out []System
	err := c.do(ctx, http.MethodGet, "/api/systems?q="+url.QueryEscape(q), nil, &out)
	return out, err
}

// SendJob calls POST /api/jobs with one report.
func (c *Client) SendJob(ctx context.Context, j JobIn) error {
	return c.do(ctx, http.MethodPost, "/api/jobs", j, nil)
}

// Login checks tok with the API and saves the link at path. It returns who the token belongs to.
func Login(ctx context.Context, path, baseURL, tok string, hc *http.Client, now time.Time) (Me, error) {
	tok = strings.TrimSpace(tok)
	if err := CheckToken(tok); err != nil {
		return Me{}, err
	}
	if err := CheckURL(baseURL); err != nil {
		return Me{}, err
	}
	me, err := (&Client{URL: baseURL, Token: tok, HTTP: hc}).Whoami(ctx)
	if err != nil {
		return Me{}, err
	}
	return me, Save(path, Link{URL: baseURL, Token: tok, LinkedAt: now.UTC()})
}

// Whoami loads the link at path and asks the API who it belongs to.
func Whoami(ctx context.Context, path string, hc *http.Client) (Me, Link, error) {
	l, ok, err := Load(path)
	if err != nil {
		return Me{}, Link{}, err
	}
	if !ok {
		return Me{}, Link{}, ErrNotLinked
	}
	me, err := (&Client{URL: l.URL, Token: l.Token, HTTP: hc}).Whoami(ctx)
	return me, l, err
}
