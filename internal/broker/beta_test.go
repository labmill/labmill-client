package broker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// betaCall makes one streaming call through BetaMessages and returns the
// accumulated text.
func betaCall(t *testing.T, c *Client) (string, error) {
	t.Helper()
	svc, opts, err := c.BetaMessages()
	if err != nil {
		t.Fatal(err)
	}
	stream := svc.NewStreaming(context.Background(), anthropic.BetaMessageNewParams{
		Model: ModelPlaceholder, MaxTokens: 16,
		Messages: []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("hi"))},
	}, opts...)
	defer stream.Close()
	var text strings.Builder
	for stream.Next() {
		ev := stream.Current()
		if ev.Type == "content_block_delta" {
			text.WriteString(ev.Delta.Text)
		}
	}
	return text.String(), stream.Err()
}

// BetaMessages calls carry the per-request token and nothing from the
// environment.
func TestBetaMessagesAuth(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-leak")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "leak-token")
	var got seen
	srv := fakeBroker(t, http.StatusOK, sse("Hel", "lo"), &got)
	var n atomic.Int32
	c, err := New(Config{BaseURL: srv.URL, Token: func(context.Context) (string, error) {
		n.Add(1)
		return "tok-" + string(rune('0'+n.Load())), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"Bearer tok-1", "Bearer tok-2"} {
		text, err := betaCall(t, c)
		if err != nil || text != "Hello" {
			t.Fatalf("call %d: %q, %v", i, text, err)
		}
		if got.auth != want || got.apiKey != "" || got.path != "/v1/messages" {
			t.Errorf("call %d: auth %q, api key %q, path %q", i, got.auth, got.apiKey, got.path)
		}
	}
}

func TestBetaMessagesTokenError(t *testing.T) {
	var got seen
	srv := fakeBroker(t, http.StatusOK, sse("x"), &got)
	c, _ := New(Config{BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "", errors.New("not signed in") }})
	if _, err := betaCall(t, c); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("err = %v", err)
	}
	if got.path != "" {
		t.Error("the broker was contacted without a token")
	}
}

// A 401 calls OnUnauthorized and retries once with a fresh token.
func TestBetaMessagesRetries401Once(t *testing.T) {
	var calls atomic.Int32
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		auths = append(auths, r.Header.Get("Authorization"))
		if !strings.Contains(string(b), `"hi"`) {
			t.Errorf("request %d lost its body: %s", calls.Load(), b)
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid token"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse("ok"))
	}))
	t.Cleanup(srv.Close)
	tok := "old"
	invalidated := 0
	c, _ := New(Config{BaseURL: srv.URL, Token: func(context.Context) (string, error) { return tok, nil },
		OnUnauthorized: func() { invalidated++; tok = "new" }})
	text, err := betaCall(t, c)
	if err != nil || text != "ok" {
		t.Fatalf("%q, %v", text, err)
	}
	if invalidated != 1 || strings.Join(auths, ",") != "Bearer old,Bearer new" {
		t.Errorf("invalidated %d, auths %v", invalidated, auths)
	}

	// A second 401 is returned, not retried again.
	calls.Store(-5)
	auths = nil
	srv401 := fakeBroker(t, http.StatusUnauthorized, "", &seen{})
	c2, _ := New(Config{BaseURL: srv401.URL, Token: staticToken("x"), OnUnauthorized: func() { invalidated++ }})
	if _, err := betaCall(t, c2); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v", err)
	}
	if invalidated != 2 {
		t.Errorf("OnUnauthorized calls = %d, want 2", invalidated)
	}
}

func TestBetaMessagesRefusesRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/v1/messages", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	c, _ := New(Config{BaseURL: srv.URL, Token: staticToken("x")})
	if _, err := betaCall(t, c); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("err = %v", err)
	}
}
