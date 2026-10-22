// Package broker is the client for an optional Labmill broker: an HTTPS
// service that speaks the Anthropic Messages API (streaming), holds the
// model credentials, authenticates the user by a bearer token, and chooses
// the model. With a broker the laptop never holds model credentials and
// never picks the model; it only sends a task hint. Without one, package
// launch calls the Anthropic API directly with the user's own key.
package broker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Hint tells the broker what kind of turn this is, so it can pick the model
// (e.g. Opus for planning, Sonnet for routine turns). Sent as HintHeader.
type Hint string

const (
	HintPlan    Hint = "plan"    // first turn of a user request: understand and plan
	HintRoutine Hint = "routine" // follow-up turns after tool results
)

// HintHeader carries the Hint on every request.
const HintHeader = "X-Labmill-Task"

// Headers about the cluster know-how (docs/dev/broker-knowledge.md).
const (
	SiteHeader          = "X-Labmill-Site"
	SkillsHeader        = "X-Labmill-Skills"
	SkillsCurrentHeader = "X-Labmill-Skills-Current"
)

// ModelPlaceholder is sent in the request's model field. The broker ignores it
// and substitutes the model it chose.
const ModelPlaceholder = "labmill-broker"

// TokenSource returns the bearer token for the current user. It is called for every request, so it can refresh.
type TokenSource func(ctx context.Context) (string, error)

// Config configures a Client.
type Config struct {
	BaseURL    string       // broker endpoint (API root, without the stage), e.g. https://abc123.execute-api.us-west-2.amazonaws.com/
	Token      TokenSource  // required
	HTTPClient *http.Client // optional; nil means a default client
	UserAgent  string       // optional, e.g. "labmill/0.1"
	// OnUnauthorized, when set, is called when the broker answers 401 before
	// any stream bytes were read; the request is then retried once with a
	// fresh Token(ctx) (the hook should make the token source drop what it
	// cached).
	OnUnauthorized func()
	// Site and SkillsVersion, when set, are sent on every request as
	// SiteHeader and SkillsHeader: the site domain and the version of the
	// cluster know-how this client uses (see internal/knowledge).
	Site          string
	SkillsVersion string
	// OnSkillsCurrent, when set, is called with the value of the response
	// header SkillsCurrentHeader (the broker's current know-how version)
	// whenever a response carries it. It runs on the request's goroutine and
	// must not block.
	OnSkillsCurrent func(version string)
}

// Request is one Messages API call.
//
// Deprecated: the agent runs on the SDK's tool runner over BetaMessages;
// Request and Stream remain for the live broker check (TestLiveBroker).
type Request struct {
	System    string
	Messages  []anthropic.MessageParam
	Tools     []anthropic.ToolUnionParam
	MaxTokens int64 // 0 means 4096
	Hint      Hint  // "" means HintRoutine
}

// Client calls the broker.
type Client struct {
	cfg    Config
	http   *http.Client
	client anthropic.Client
}

// New validates cfg and returns a Client. BaseURL must be an absolute https
// URL; plain http is accepted only for loopback hosts (localhost, 127.0.0.0/8,
// ::1), so the bearer token never travels in cleartext. Token must be non-nil.
// When HTTPClient is nil, the default client refuses redirects (Go would keep
// the Authorization header on some redirects).
func New(cfg Config) (*Client, error) {
	if cfg.Token == nil {
		return nil, errors.New("broker: Token source is required")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, fmt.Errorf("broker: BaseURL %q must be an absolute https URL", cfg.BaseURL)
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return nil, fmt.Errorf("broker: BaseURL %q uses plain http; https is required except for loopback hosts", cfg.BaseURL)
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	}
	// WithoutEnvironmentDefaults stops the SDK from reading ANTHROPIC_API_KEY,
	// ANTHROPIC_AUTH_TOKEN, ANTHROPIC_BASE_URL, ANTHROPIC_PROFILE, federation
	// variables, custom headers and the profile config file. Only the options
	// below (and the per-request token) shape requests.
	opts := []option.RequestOption{
		option.WithoutEnvironmentDefaults(),
		option.WithHTTPClient(hc),
		option.WithBaseURL(cfg.BaseURL),
		option.WithMaxRetries(0),
	}
	if cfg.UserAgent != "" {
		opts = append(opts, option.WithHeader("User-Agent", cfg.UserAgent))
	}
	return &Client{cfg: cfg, http: hc, client: anthropic.NewClient(opts...)}, nil
}

// setKnowledgeHeaders adds SiteHeader and SkillsHeader to r.
func (c *Client) setKnowledgeHeaders(r *http.Request) {
	if c.cfg.Site != "" {
		r.Header.Set(SiteHeader, c.cfg.Site)
	}
	if c.cfg.SkillsVersion != "" {
		r.Header.Set(SkillsHeader, c.cfg.SkillsVersion)
	}
}

// observe hands the broker's current know-how version to OnSkillsCurrent.
func (c *Client) observe(resp *http.Response) {
	if c.cfg.OnSkillsCurrent == nil || resp == nil {
		return
	}
	if v := strings.TrimSpace(resp.Header.Get(SkillsCurrentHeader)); v != "" {
		c.cfg.OnSkillsCurrent(v)
	}
}

// isLoopback reports whether host is localhost or a loopback IP address.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// BetaMessages returns the SDK's Beta Messages service for the broker (the
// same base URL, no credentials from the environment, no SDK retries,
// redirects refused) and the request options every call on it must carry.
// The options hold one middleware that, per HTTP request:
//   - sets "Authorization: Bearer <token>" from cfg.Token (a token-source
//     error fails the request without contacting the broker);
//   - refuses a 3xx answer with an error;
//   - on a 401, when cfg.OnUnauthorized is set, calls it and retries the
//     request once with a fresh token.
//
// The agent's tool runner (client.Beta.Messages.NewToolRunnerStreaming)
// runs on it and adds HintHeader per request itself. The error is always
// nil here; it lets a model that has no broker say why.
func (c *Client) BetaMessages() (*anthropic.BetaMessageService, []option.RequestOption, error) {
	return &c.client.Beta.Messages, []option.RequestOption{option.WithMiddleware(c.authMiddleware)}, nil
}

// authMiddleware implements the per-request part of BetaMessages.
func (c *Client) authMiddleware(r *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	token, err := c.cfg.Token(r.Context())
	if err != nil {
		return nil, fmt.Errorf("broker: token: %w", err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	c.setKnowledgeHeaders(r)
	resp, err := next(r)
	if err != nil {
		return resp, err
	}
	c.observe(resp)
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("broker: the broker redirected (HTTP %d) and redirects are refused", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusUnauthorized || c.cfg.OnUnauthorized == nil || r.GetBody == nil {
		return resp, nil
	}
	body, err := r.GetBody()
	if err != nil {
		return resp, nil
	}
	resp.Body.Close()
	c.cfg.OnUnauthorized()
	token, err = c.cfg.Token(r.Context())
	if err != nil {
		body.Close()
		return nil, fmt.Errorf("broker: token: %w", err)
	}
	retry := r.Clone(r.Context())
	retry.Body = body
	retry.Header.Set("Authorization", "Bearer "+token)
	resp, err = next(retry)
	if err == nil {
		c.observe(resp)
	}
	if err == nil && resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("broker: the broker redirected (HTTP %d) and redirects are refused", resp.StatusCode)
	}
	return resp, err
}

// Stream sends req as a streaming Messages request and calls onText (if not
// nil) with each text delta as it arrives, so the UI can render the reply
// live. It returns the fully accumulated message.
//
// Every request carries "Authorization: Bearer <token>" from cfg.Token and
// HintHeader. It must never send credentials from the environment: an
// ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN set in the user's environment must
// not leak to the broker, and ANTHROPIC_BASE_URL must not redirect requests.
// The SDK's automatic retries are disabled (MaxRetries 0); the agent decides
// what to retry. A token-source error is returned without contacting the
// broker. HTTP errors come back as errors that include the status code. A
// stream that ends without a message_stop event (the broker closed the
// response part-way) is an error, never a partial success. There is no
// built-in timeout: callers pass a cancelable ctx.
//
// Deprecated: use BetaMessages (the agent's tool runner does); Stream
// remains for the live broker check (TestLiveBroker).
func (c *Client) Stream(ctx context.Context, req Request, onText func(string)) (*anthropic.Message, error) {
	msg, started, err := c.stream(ctx, req, onText)
	var apiErr *anthropic.Error
	if err != nil && !started && c.cfg.OnUnauthorized != nil && errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
		c.cfg.OnUnauthorized()
		msg, _, err = c.stream(ctx, req, onText)
	}
	return msg, err
}

// stream makes one request. started reports whether any stream event was
// read, after which a retry would repeat output.
func (c *Client) stream(ctx context.Context, req Request, onText func(string)) (msg *anthropic.Message, started bool, err error) {
	token, err := c.cfg.Token(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("broker: token: %w", err)
	}
	hint := req.Hint
	if hint == "" {
		hint = HintRoutine
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(ModelPlaceholder),
		MaxTokens: maxTokens,
		Messages:  req.Messages,
		Tools:     req.Tools,
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}
	// The default client refuses redirects and hands the 3xx response back as
	// is; remember its status so an empty stream can be explained.
	redirect := 0
	stream := c.client.Messages.NewStreaming(ctx, params,
		option.WithAuthToken(token),
		option.WithHeader(HintHeader, string(hint)),
		option.WithMiddleware(func(r *http.Request, next option.MiddlewareNext) (*http.Response, error) {
			c.setKnowledgeHeaders(r)
			resp, err := next(r)
			if err == nil {
				c.observe(resp)
			}
			if err == nil && resp != nil && resp.StatusCode >= 300 && resp.StatusCode < 400 {
				redirect = resp.StatusCode
			}
			return resp, err
		}),
	)
	defer stream.Close()
	// NewStreaming has already made the request: report a redirect before
	// reading any events, so a decode error on the 3xx body can't mask it.
	if redirect != 0 {
		return nil, false, fmt.Errorf("broker: the broker redirected (HTTP %d) and redirects are refused", redirect)
	}

	var m anthropic.Message
	stopped := false
	for stream.Next() {
		started = true
		event := stream.Current()
		if _, ok := event.AsAny().(anthropic.MessageStopEvent); ok {
			stopped = true
		}
		if err := m.Accumulate(event); err != nil {
			return nil, started, fmt.Errorf("broker: %w", err)
		}
		if onText == nil {
			continue
		}
		if d, ok := event.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
			if td, ok := d.Delta.AsAny().(anthropic.TextDelta); ok {
				onText(td.Text)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return nil, started, fmt.Errorf("broker: %w", err)
	}
	if !stopped {
		return nil, started, errors.New("broker: stream ended before message_stop")
	}
	return &m, started, nil
}
