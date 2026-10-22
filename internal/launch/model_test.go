package launch

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"labmill-client/internal/broker"
)

// The direct model puts the planning or routine model into the body and
// drops the broker's hint header; the rest of the body stays.
func TestDirectModelMiddleware(t *testing.T) {
	if _, err := newDirectModel(" ", func(string) string { return "" }); err == nil {
		t.Error("an empty key was accepted")
	}
	m, err := newDirectModel("k", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	for hint, want := range map[broker.Hint]string{broker.HintPlan: string(directPlanModel), broker.HintRoutine: string(directRoutineModel), "": string(directRoutineModel)} {
		r, _ := http.NewRequest(http.MethodPost, "https://api.example/v1/messages", bytes.NewReader([]byte(`{"model":"labmill-broker","max_tokens":5}`)))
		if hint != "" {
			r.Header.Set(broker.HintHeader, string(hint))
		}
		var got map[string]any
		_, err := m.middleware(r, option.MiddlewareNext(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get(broker.HintHeader) != "" {
				t.Error("hint header sent")
			}
			b, _ := io.ReadAll(r.Body)
			if int64(len(b)) != r.ContentLength {
				t.Errorf("content length %d for %d bytes", r.ContentLength, len(b))
			}
			return nil, json.Unmarshal(b, &got)
		}))
		if err != nil || got["model"] != want || got["max_tokens"] != float64(5) {
			t.Errorf("hint %q: body %v, err %v", hint, got, err)
		}
	}
	m, _ = newDirectModel("k", func(string) string { return "claude-x" })
	if m.plan != "claude-x" || m.normal != "claude-x" {
		t.Errorf("LABMILL_MODEL not used: %q %q", m.plan, m.normal)
	}
}
