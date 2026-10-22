package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// sse renders a minimal Messages streaming response with the given text deltas.
func sse(deltas ...string) string {
	var b strings.Builder
	ev := func(name, data string) { fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", name, data) }
	ev("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}`)
	ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	for _, d := range deltas {
		ev("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, d))
	}
	ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
	ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`)
	ev("message_stop", `{"type":"message_stop"}`)
	return b.String()
}

type seen struct {
	auth, hint, apiKey, path, body string
}

func fakeBroker(t *testing.T, status int, body string, got *seen) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = seen{auth: r.Header.Get("Authorization"), hint: r.Header.Get(HintHeader), apiKey: r.Header.Get("X-Api-Key"), path: r.URL.Path, body: string(b)}
		if status != http.StatusOK {
			w.WriteHeader(status)
			io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"bad token"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func staticToken(tok string) TokenSource {
	return func(context.Context) (string, error) { return tok, nil }
}

func TestStream(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-leak")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "leak-token")
	var got seen
	srv := fakeBroker(t, http.StatusOK, sse("Hel", "lo"), &got)
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1") // must be ignored

	c, err := New(Config{BaseURL: srv.URL, Token: staticToken("test-tok")})
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	msg, err := c.Stream(context.Background(), Request{
		System:   "sys",
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
		Hint:     HintPlan,
	}, func(s string) { deltas = append(deltas, s) })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if strings.Join(deltas, "|") != "Hel|lo" {
		t.Errorf("deltas = %v", deltas)
	}
	if len(msg.Content) != 1 || msg.Content[0].Text != "Hello" || msg.StopReason != anthropic.StopReasonEndTurn {
		t.Errorf("message = %+v", msg)
	}
	if got.auth != "Bearer test-tok" {
		t.Errorf("Authorization = %q", got.auth)
	}
	if got.hint != "plan" {
		t.Errorf("hint = %q", got.hint)
	}
	if strings.Contains(got.apiKey, "leak") || strings.Contains(got.auth, "leak") {
		t.Errorf("environment credentials leaked: apiKey=%q auth=%q", got.apiKey, got.auth)
	}
	if !strings.HasSuffix(got.path, "/v1/messages") {
		t.Errorf("path = %q", got.path)
	}
	for _, want := range []string{`"model":"labmill-broker"`, `"stream":true`, `"max_tokens":4096`, `"sys"`} {
		if !strings.Contains(got.body, want) {
			t.Errorf("request body missing %s: %s", want, got.body)
		}
	}
}

func TestStreamDefaultsHintRoutine(t *testing.T) {
	var got seen
	srv := fakeBroker(t, http.StatusOK, sse("x"), &got)
	c, _ := New(Config{BaseURL: srv.URL, Token: staticToken("t")})
	if _, err := c.Stream(context.Background(), Request{Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}, nil); err != nil {
		t.Fatal(err)
	}
	if got.hint != "routine" {
		t.Errorf("hint = %q", got.hint)
	}
}

func TestStreamHTTPError(t *testing.T) {
	var got seen
	srv := fakeBroker(t, http.StatusUnauthorized, "", &got)
	c, _ := New(Config{BaseURL: srv.URL, Token: staticToken("t")})
	_, err := c.Stream(context.Background(), Request{Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}, nil)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want one mentioning 401", err)
	}
}

func TestStreamTokenError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	boom := errors.New("not logged in")
	c, _ := New(Config{BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "", boom }})
	_, err := c.Stream(context.Background(), Request{Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 0 {
		t.Error("broker contacted despite token error")
	}
}

func TestNewValidates(t *testing.T) {
	for _, cfg := range []Config{
		{BaseURL: "", Token: staticToken("t")},
		{BaseURL: "not a url", Token: staticToken("t")},
		{BaseURL: "ftp://x", Token: staticToken("t")},
		{BaseURL: "https://x"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) succeeded", cfg)
		}
	}
}

func TestStreamRefusedRedirectReportsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.invalid/", http.StatusFound)
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, Token: staticToken("t")})
	_, err := c.Stream(context.Background(), Request{Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}, nil)
	if err == nil || !strings.Contains(err.Error(), "redirected (HTTP 302)") {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamRetriesOnceOn401(t *testing.T) {
	var calls atomic.Int32
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		auths = append(auths, r.Header.Get("Authorization"))
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"expired"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse("ok"))
	}))
	defer srv.Close()
	tok := "old"
	hooks := 0
	c, _ := New(Config{BaseURL: srv.URL, Token: func(context.Context) (string, error) { return tok, nil },
		OnUnauthorized: func() { hooks++; tok = "new" }})
	req := Request{Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
	if _, err := c.Stream(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if hooks != 1 || calls.Load() != 2 || auths[0] != "Bearer old" || auths[1] != "Bearer new" {
		t.Fatalf("hooks=%d calls=%d auths=%v", hooks, calls.Load(), auths)
	}
}

func TestStream401RetriesOnlyOnce(t *testing.T) {
	var got seen
	srv := fakeBroker(t, http.StatusUnauthorized, "", &got)
	hooks := 0
	c, _ := New(Config{BaseURL: srv.URL, Token: staticToken("t"), OnUnauthorized: func() { hooks++ }})
	_, err := c.Stream(context.Background(), Request{Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}, nil)
	if err == nil || !strings.Contains(err.Error(), "401") || hooks != 1 {
		t.Fatalf("err=%v hooks=%d", err, hooks)
	}
}
