// Package agenttest is a fake Labmill broker for tests: an httptest server
// that speaks the streaming Messages API the way the real broker does and
// answers each POST /v1/messages with the next scripted assistant message,
// converted to server-sent events. Tests point a *broker.Client at it
// (Client), so the agent's tool runner runs unchanged, and inspect the
// recorded requests (Requests).
package agenttest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"labmill-client/internal/broker"
)

// Token is the bearer token Client sends.
const Token = "agenttest-token"

// Request is one recorded Messages request.
type Request struct {
	Hint   broker.Hint    // the X-Labmill-Task header
	Betas  []string       // the anthropic-beta header values, split on commas
	System string         // the system blocks' text, concatenated
	Tools  int            // number of tool definitions
	Body   map[string]any // the decoded JSON body
	Auth   string         // the Authorization header
}

// Raw marks a reply that is sent as is (a raw SSE body, e.g. a stream that
// ends early) instead of being converted from a message.
const Raw = "SSE:"

// Broker is the fake broker.
type Broker struct {
	URL string

	mu      sync.Mutex
	replies []string
	reqs    []Request
	reply   func(n int, req Request) (string, error)
}

// New starts a fake broker that answers with replies in order: each is the
// JSON of an assistant message (as the non-streaming API returns it), or
// Raw followed by a literal SSE body. Past the last reply it answers HTTP
// 500. The server stops when the test ends.
func New(t testing.TB, replies ...string) *Broker {
	t.Helper()
	b := &Broker{replies: replies}
	srv := httptest.NewServer(http.HandlerFunc(b.serve))
	t.Cleanup(srv.Close)
	b.URL = srv.URL
	return b
}

// NewFunc is New with replies computed per request: n is the 0-based
// request number. A returned error is answered with HTTP 500.
func NewFunc(t testing.TB, reply func(n int, req Request) (string, error)) *Broker {
	b := New(t)
	b.reply = reply
	return b
}

// Client returns a broker client for b (fixed token Token).
func (b *Broker) Client(t testing.TB) *broker.Client {
	t.Helper()
	c, err := broker.New(broker.Config{BaseURL: b.URL, Token: func(context.Context) (string, error) { return Token, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Requests returns a copy of the requests received so far.
func (b *Broker) Requests() []Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Request(nil), b.reqs...)
}

func (b *Broker) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		http.NotFound(w, r)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	req := Request{Hint: broker.Hint(r.Header.Get(broker.HintHeader)), Auth: r.Header.Get("Authorization")}
	for _, v := range r.Header.Values("Anthropic-Beta") {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				req.Betas = append(req.Betas, s)
			}
		}
	}
	if err := json.Unmarshal(raw, &req.Body); err != nil {
		apiError(w, http.StatusBadRequest, "body is not JSON")
		return
	}
	switch sys := req.Body["system"].(type) {
	case string:
		req.System = sys
	case []any:
		for _, blk := range sys {
			if m, ok := blk.(map[string]any); ok {
				s, _ := m["text"].(string)
				req.System += s
			}
		}
	}
	if tools, ok := req.Body["tools"].([]any); ok {
		req.Tools = len(tools)
	}

	b.mu.Lock()
	n := len(b.reqs)
	b.reqs = append(b.reqs, req)
	var reply string
	var err error
	switch {
	case b.reply != nil:
		b.mu.Unlock()
		reply, err = b.reply(n, req)
	case n < len(b.replies):
		reply = b.replies[n]
		b.mu.Unlock()
	default:
		b.mu.Unlock()
		err = fmt.Errorf("no more replies")
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var body string
	if s, ok := strings.CutPrefix(reply, Raw); ok {
		body = s
	} else if body, err = SSE(reply); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, body)
}

func apiError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": msg}})
}

// SSE converts the JSON of an assistant message into the event stream the
// broker would send, shaped like the real one: message_start (no content,
// no stop reason), then per content block a content_block_start, deltas and
// content_block_stop, then message_delta with the stop reason and usage, and
// message_stop. Text blocks start empty and get one text_delta. tool_use
// blocks start with input {} and get the input's exact bytes as up to three
// input_json_delta chunks. thinking blocks start empty and get a
// thinking_delta (when the text is not empty) and a signature_delta. Other
// blocks (e.g. compaction) start complete.
func SSE(message string) (string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(message), &m); err != nil {
		return "", fmt.Errorf("agenttest: reply is not a JSON message: %w", err)
	}
	var content []json.RawMessage
	if c, ok := m["content"]; ok {
		if err := json.Unmarshal(c, &content); err != nil {
			return "", fmt.Errorf("agenttest: content is not an array: %w", err)
		}
	}
	stop := m["stop_reason"]
	if stop == nil {
		stop = json.RawMessage("null")
	}
	var usage map[string]json.RawMessage
	json.Unmarshal(m["usage"], &usage)
	start := map[string]json.RawMessage{}
	for k, v := range m {
		start[k] = v
	}
	start["content"] = json.RawMessage("[]")
	start["stop_reason"] = json.RawMessage("null")
	start["stop_sequence"] = json.RawMessage("null")

	var sb strings.Builder
	event := func(v map[string]any) {
		js, _ := json.Marshal(v)
		fmt.Fprintf(&sb, "event: %s\ndata: %s\n\n", v["type"], js)
	}
	delta := func(i int, d map[string]any) {
		event(map[string]any{"type": "content_block_delta", "index": i, "delta": d})
	}
	event(map[string]any{"type": "message_start", "message": start})
	for i, c := range content {
		var blk map[string]json.RawMessage
		if err := json.Unmarshal(c, &blk); err != nil {
			return "", fmt.Errorf("agenttest: content block %d is not an object: %w", i, err)
		}
		var typ string
		json.Unmarshal(blk["type"], &typ)
		switch typ {
		case "text":
			var text string
			json.Unmarshal(blk["text"], &text)
			event(map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "text", "text": ""}})
			if text != "" {
				delta(i, map[string]any{"type": "text_delta", "text": text})
			}
		case "tool_use":
			first := map[string]json.RawMessage{}
			for k, v := range blk {
				first[k] = v
			}
			first["input"] = json.RawMessage("{}")
			event(map[string]any{"type": "content_block_start", "index": i, "content_block": first})
			in := string(blk["input"])
			if in == "" {
				in = "{}"
			}
			for _, chunk := range thirds(in) {
				delta(i, map[string]any{"type": "input_json_delta", "partial_json": chunk})
			}
		case "thinking":
			var thinking, sig string
			json.Unmarshal(blk["thinking"], &thinking)
			json.Unmarshal(blk["signature"], &sig)
			event(map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}})
			if thinking != "" {
				delta(i, map[string]any{"type": "thinking_delta", "thinking": thinking})
			}
			delta(i, map[string]any{"type": "signature_delta", "signature": sig})
		default:
			event(map[string]any{"type": "content_block_start", "index": i, "content_block": blk})
		}
		event(map[string]any{"type": "content_block_stop", "index": i})
	}
	du := map[string]json.RawMessage{"output_tokens": json.RawMessage("0")}
	for k, v := range usage {
		du[k] = v
	}
	event(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": du})
	event(map[string]any{"type": "message_stop"})
	return sb.String(), nil
}

// thirds splits s into up to three non-empty pieces (on byte boundaries
// that keep UTF-8 sequences whole).
func thirds(s string) []string {
	if len(s) < 3 {
		return []string{s}
	}
	var out []string
	cut := func(n int) int {
		for n < len(s) && !utf8.RuneStart(s[n]) {
			n++
		}
		return n
	}
	a, b := cut(len(s)/3), cut(2*len(s)/3)
	for _, p := range []string{s[:a], s[a:b], s[b:]} {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// TextReply is the JSON of an end_turn message with one text block.
func TextReply(text string) string {
	b, _ := json.Marshal(text)
	return `{"id":"m","type":"message","role":"assistant","model":"x","stop_reason":"end_turn","content":[{"type":"text","text":` + string(b) + `}],"usage":{"input_tokens":1,"output_tokens":1}}`
}

// ToolUse is one tool call for ToolReply.
type ToolUse struct {
	ID, Name string
	Input    string // JSON object; "" means {}
}

// ToolReply is the JSON of a tool_use message with the given calls.
func ToolReply(calls ...ToolUse) string {
	var blocks []string
	for _, c := range calls {
		in := c.Input
		if in == "" {
			in = "{}"
		}
		id, _ := json.Marshal(c.ID)
		name, _ := json.Marshal(c.Name)
		blocks = append(blocks, `{"type":"tool_use","id":`+string(id)+`,"name":`+string(name)+`,"input":`+in+`}`)
	}
	return `{"id":"m","type":"message","role":"assistant","model":"x","stop_reason":"tool_use","content":[` + strings.Join(blocks, ",") + `],"usage":{"input_tokens":1,"output_tokens":1}}`
}
