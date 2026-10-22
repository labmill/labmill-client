//go:build !race

// Shared by the live agent tests (the !race tag matches theirs).

package launch

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"labmill-client/internal/agent"
)

// countingModel counts model calls (HTTP requests to the broker).
type countingModel struct {
	inner agent.Model
	n     int
}

func (m *countingModel) BetaMessages() (*anthropic.BetaMessageService, []option.RequestOption, error) {
	svc, opts, err := m.inner.BetaMessages()
	count := option.WithMiddleware(func(r *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		m.n++
		return next(r)
	})
	return svc, append([]option.RequestOption{count}, opts...), err
}

// between returns the text after the first occurrence of start and before
// the next occurrence of end.
func between(s, start, end string) (string, bool) {
	i := strings.Index(s, start)
	if i < 0 {
		return "", false
	}
	s = s[i+len(start):]
	j := strings.Index(s, end)
	if j < 0 {
		return "", false
	}
	return s[:j], true
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("... (%d more bytes)", len(s)-n)
}
