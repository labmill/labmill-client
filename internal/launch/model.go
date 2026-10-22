package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"labmill-client/internal/broker"
)

// Model choice when the client calls the Anthropic Messages API itself
// (no broker): the planning request gets the larger model, every other
// request the routine one. LABMILL_MODEL, when set, is used for both.
const (
	directPlanModel    = anthropic.ModelClaudeOpus5_5
	directRoutineModel = anthropic.ModelClaudeSonnet5_5
)

// directModel is the agent.Model that calls the Anthropic Messages API with
// the user's own API key (ANTHROPIC_API_KEY, read from the environment and
// kept in memory only). It replaces the broker until accounts exist.
type directModel struct {
	client anthropic.Client
	plan   anthropic.Model
	normal anthropic.Model
}

// newDirectModel returns the direct model for key, or an error when key is
// empty. getenv reads LABMILL_MODEL.
func newDirectModel(key string, getenv func(string) string) (*directModel, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("no ANTHROPIC_API_KEY")
	}
	m := &directModel{plan: directPlanModel, normal: directRoutineModel}
	if v := strings.TrimSpace(getenv("LABMILL_MODEL")); v != "" {
		m.plan, m.normal = anthropic.Model(v), anthropic.Model(v)
	}
	// WithoutEnvironmentDefaults: only the key passed here is used, and
	// ANTHROPIC_BASE_URL can't send the key somewhere else.
	m.client = anthropic.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithAPIKey(key),
		option.WithMaxRetries(0),
		option.WithHeader("User-Agent", "labmill/"+Version),
	)
	return m, nil
}

// BetaMessages implements agent.Model. The middleware puts the real model
// into the request body (the agent sends broker.ModelPlaceholder) and drops
// the broker's hint header.
func (m *directModel) BetaMessages() (*anthropic.BetaMessageService, []option.RequestOption, error) {
	return &m.client.Beta.Messages, []option.RequestOption{option.WithMiddleware(m.middleware)}, nil
}

func (m *directModel) middleware(r *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	model := m.normal
	if broker.Hint(r.Header.Get(broker.HintHeader)) == broker.HintPlan {
		model = m.plan
	}
	r.Header.Del(broker.HintHeader)
	if err := setModel(r, model); err != nil {
		return nil, err
	}
	return next(r)
}

// setModel replaces the "model" field of r's JSON body.
func setModel(r *http.Request, model anthropic.Model) error {
	if r.Body == nil {
		return nil
	}
	data, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		return err
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(data, &body); err != nil {
		return errors.New("model request: the body is not a JSON object")
	}
	name, _ := json.Marshal(string(model))
	body["model"] = name
	out, err := json.Marshal(body)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(out))
	r.ContentLength = int64(len(out))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(out)), nil }
	return nil
}

// envToken is the broker's bearer token (a broker.TokenSource) when a broker
// is configured: LABMILL_BROKER_TOKEN. There is no sign-in of its own yet.
func envToken(context.Context) (string, error) {
	if t := strings.TrimSpace(os.Getenv("LABMILL_BROKER_TOKEN")); t != "" {
		return t, nil
	}
	return "", errors.New("no broker token: set LABMILL_BROKER_TOKEN")
}

// hasEnvToken reports whether envToken would succeed now.
func hasEnvToken() bool {
	_, err := envToken(context.Background())
	return err == nil
}
