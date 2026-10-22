package broker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func userReq() Request {
	return Request{Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
}

func TestReviewTruncatedStream(t *testing.T) {
	full := sse("Hel", "lo")
	body := full[:strings.Index(full, "event: message_delta")]
	var got seen
	srv := fakeBroker(t, http.StatusOK, body, &got)
	c, err := New(Config{BaseURL: srv.URL, Token: staticToken("t")})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := c.Stream(context.Background(), userReq(), nil)
	if err == nil || !strings.Contains(err.Error(), "message_stop") {
		t.Fatalf("msg=%v err=%v, want message_stop error", msg, err)
	}
}

func TestReviewNewRequiresHTTPSExceptLoopback(t *testing.T) {
	tok := staticToken("t")
	for _, u := range []string{"http://example.com", "http://10.0.0.1:8080", "http://localhost.evil.com"} {
		if _, err := New(Config{BaseURL: u, Token: tok}); err == nil {
			t.Errorf("New(%q) succeeded, want error", u)
		}
	}
	for _, u := range []string{"https://example.com/x", "http://127.0.0.1:8080", "http://127.5.5.5", "http://localhost:9", "http://[::1]:9"} {
		if _, err := New(Config{BaseURL: u, Token: tok}); err != nil {
			t.Errorf("New(%q): %v", u, err)
		}
	}
}

func TestReviewRefusesRedirect(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, "{}")
	}))
	t.Cleanup(target.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, Token: staticToken("secret")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stream(context.Background(), userReq(), nil); err == nil {
		t.Fatal("Stream followed/accepted a redirect")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("redirect target was contacted %d times", n)
	}
}
