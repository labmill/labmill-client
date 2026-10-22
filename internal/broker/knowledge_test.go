package broker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKnowledgeHeadersAndCurrent(t *testing.T) {
	var gotSite, gotSkills string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSite, gotSkills = r.Header.Get(SiteHeader), r.Header.Get(SkillsHeader)
		w.Header().Set(SkillsCurrentHeader, "newer")
		io.WriteString(w, `{"site":"a.edu","version":"v1","index":true,"files":[]}`)
	}))
	defer srv.Close()
	var current []string
	c, err := New(Config{BaseURL: srv.URL, Token: staticToken("t"), Site: "a.edu", SkillsVersion: "v0",
		OnSkillsCurrent: func(v string) { current = append(current, v) }})
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.Manifest(context.Background())
	if err != nil || m.Version != "v1" || !m.Index {
		t.Fatalf("manifest %+v, %v", m, err)
	}
	if gotSite != "a.edu" || gotSkills != "v0" || len(current) != 1 || current[0] != "newer" {
		t.Errorf("site=%q skills=%q current=%v", gotSite, gotSkills, current)
	}

	// The messages middleware sets the same headers and reports the answer.
	gotSite, gotSkills, current = "", "", nil
	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader("{}"))
	resp, err := c.authMiddleware(req, func(r *http.Request) (*http.Response, error) {
		gotSite, gotSkills = r.Header.Get(SiteHeader), r.Header.Get(SkillsHeader)
		return &http.Response{StatusCode: 200, Header: http.Header{SkillsCurrentHeader: {"n2"}}, Body: http.NoBody}, nil
	})
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(err)
	}
	if gotSite != "a.edu" || gotSkills != "v0" || len(current) != 1 || current[0] != "n2" {
		t.Errorf("middleware: site=%q skills=%q current=%v", gotSite, gotSkills, current)
	}
}

func TestKnowledgeStatuses(t *testing.T) {
	var status int
	var inm string
	var big bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inm = r.Header.Get("If-None-Match")
		if big {
			io.WriteString(w, strings.Repeat("x", MaxKnowledgeBytes+1))
			return
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, Token: staticToken("t")})
	ctx := context.Background()
	status = 404
	if _, err := c.Manifest(ctx); !errors.Is(err, ErrKnowledgeUnavailable) {
		t.Errorf("404: %v", err)
	}
	status = 304
	if _, err := c.Bundle(ctx, "abc", `"abc"`); !errors.Is(err, ErrNotModified) || inm != `"abc"` {
		t.Errorf("304: %v inm=%q", err, inm)
	}
	status = 500
	if _, err := c.Bundle(ctx, "abc", ""); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("500: %v", err)
	}
	big = true
	if _, err := c.Bundle(ctx, "abc", ""); err == nil {
		t.Error("oversized answer accepted")
	}
}

func TestKnowledgeRefusesPlainHTTP(t *testing.T) {
	if _, err := New(Config{BaseURL: "http://broker.example.edu", Token: staticToken("t")}); err == nil {
		t.Error("plain http to a non-loopback host accepted")
	}
}
