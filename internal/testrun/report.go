package testrun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"labmill-client/internal/account"
)

// Result is one case of a run. Field names match the server (internal/api/testruns.go).
type Result struct {
	Case         string `json:"case"`
	Outcome      string `json:"outcome"`
	FailureClass string `json:"failure_class,omitempty"`
	Seconds      int    `json:"seconds"`
	Reason       string `json:"reason,omitempty"`
}

// Report is the body of POST /api/test-runs.
type Report struct {
	RunRef        string    `json:"run_ref"`
	ClientVersion string    `json:"client_version"`
	ClusterHash   string    `json:"cluster_hash"`
	HPCSystemID   string    `json:"hpc_system_id,omitempty"` // only for a listed system and a contributing account
	Scheduler     string    `json:"scheduler"`
	StartedAt     time.Time `json:"started_at"`
	Results       []Result  `json:"results"`
}

// ClusterHash is the SHA-256 of the install salt and the alias, hex. No name or host leaves the machine.
func ClusterHash(salt, alias string) string {
	sum := sha256.Sum256([]byte(salt + "\x00" + alias))
	return hex.EncodeToString(sum[:])
}

// SystemID finds the API id of the listed system for a cluster alias. It asks
// the server only when the account contributes (a fully private account does
// not tell the server a cluster name; nil, from an older server, counts as private). Any failure gives "": the report then
// carries the hash only.
func SystemID(ctx context.Context, me func(context.Context) (account.Me, error), resolve func(context.Context, account.SystemRef) (account.System, error), cluster string) string {
	m, err := me(ctx)
	if err != nil || (m.Contributes == nil || !*m.Contributes) {
		return ""
	}
	s, err := resolve(ctx, account.SystemRef{Name: cluster})
	if err != nil {
		return ""
	}
	if !uuidRe.MatchString(s.ID) {
		return ""
	}
	return s.ID
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Post sends the report to base + "/api/test-runs" with the program token. A 204 is success,
// also for a private account (the server stores nothing).
func Post(ctx context.Context, hc *http.Client, base, token string, r Report) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/test-runs", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	return fmt.Errorf("report refused: %s: %s", resp.Status, bytes.TrimSpace(msg))
}
