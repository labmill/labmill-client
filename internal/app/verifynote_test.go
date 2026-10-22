package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"labmill-client/internal/store"
	"labmill-client/internal/transfer"
)

func TestVerifyNote(t *testing.T) {
	cases := []struct {
		name string
		v    transfer.Verification
		want []string
	}{
		{"all verified", transfer.Verification{Verified: 3}, []string{"Verified 3 files (SHA-256"}},
		{"size only", transfer.Verification{SizeOnly: "sha256sum not available on hpclogin", Unverified: 2}, []string{"verified by size only: sha256sum not available on hpclogin"}},
		{"partly", transfer.Verification{Verified: 5, Unverified: 2, SizeOnly: "timeout"}, []string{"Verified 5 files (SHA-256); the other 2 by size only (timeout)"}},
		{"mismatch", transfer.Verification{Verified: 1, Mismatched: []string{"/r/out/a.mismatch"}}, []string{"WARNING", "/r/out/a.mismatch", "fetch it again"}},
		{"failed", transfer.Verification{Failed: []string{"d/gone.txt"}}, []string{"d/gone.txt", "content not verified"}},
	}
	for _, c := range cases {
		got := verifyNote(c.v)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %q", c.name, got, w)
			}
		}
	}
	if got := verifyNote(transfer.Verification{}); got != "" {
		t.Errorf("empty verification gave %q", got)
	}
}

func TestVerifyNoteCapsLists(t *testing.T) {
	var many []string
	for i := 0; i < maxResultLines+10; i++ {
		many = append(many, fmt.Sprintf("f%d", i))
	}
	got := verifyNote(transfer.Verification{Mismatched: many, Failed: many, Unrenamed: many})
	if strings.Count(got, "... and 10 more") != 3 {
		t.Errorf("lists not capped: %q", got)
	}
}

// fetch_results: what the job's state and the result say for each outcome of
// the SHA-256 check (the fake Download calls OnVerify like the real one).
func TestFetchResultsVerificationOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		v       transfer.Verification
		fetched bool
		want    []string
	}{
		{"mismatch", transfer.Verification{Mismatched: []string{"/r/out/a.txt.mismatch"}}, false, []string{"WARNING", "/r/out/a.txt.mismatch"}},
		{"unrenamed", transfer.Verification{Unrenamed: []string{"/r/out/a.txt"}}, false, []string{"WARNING", "could not be renamed"}},
		{"failed", transfer.Verification{Failed: []string{"out/a.txt"}}, true, []string{"content not verified"}},
		{"size only", transfer.Verification{SizeOnly: "sha256sum not available on hpclogin", Unverified: 1}, true, []string{"verified by size only"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.env.Download = func(_ context.Context, src transfer.Remote, paths []string, localDir string, _ func(transfer.Progress)) ([]string, error) {
				src.OnVerify(c.v)
				return []string{filepath.Join(localDir, paths[0])}, nil
			}
			id, _ := h.env.Store.CreateJob(t.Context(), store.Job{Name: "x", Cluster: "hpcone", RemoteDir: "/home/alice/x", RemoteID: "1", State: store.StateCompleted})
			res, err := h.run(t, "fetch_results", map[string]any{"job_id": id, "paths": []string{"out/a.txt"}})
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range c.want {
				if !strings.Contains(res, w) {
					t.Errorf("result lacks %q: %q", w, res)
				}
			}
			j, _ := h.env.Store.Job(t.Context(), id)
			if c.fetched {
				if j.State != store.StateFetched || j.LocalDir == "" {
					t.Errorf("state = %s, LocalDir = %q; want fetched", j.State, j.LocalDir)
				}
			} else if j.State == store.StateFetched || j.LocalDir != "" {
				t.Errorf("state = %s, LocalDir = %q; a mismatch must not mark the job fetched", j.State, j.LocalDir)
			}
		})
	}
}
