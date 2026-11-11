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
	"time"
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
	Scheduler     string    `json:"scheduler"`
	StartedAt     time.Time `json:"started_at"`
	Results       []Result  `json:"results"`
}

// ClusterHash is the SHA-256 of the install salt and the alias, hex. No name or host leaves the machine.
func ClusterHash(salt, alias string) string {
	sum := sha256.Sum256([]byte(salt + "\x00" + alias))
	return hex.EncodeToString(sum[:])
}

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
