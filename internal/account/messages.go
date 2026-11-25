package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	"labmill-client/internal/config"
	"labmill-client/internal/textsafe"
)

// Message is one entry of GET /api/messages: a message to this person from the research computing
// team, for example the reply to a request for help (HelpRequestID). Field names are those of the
// server's message row; unknown fields are ignored.
type Message struct {
	ID            string
	Body          string
	Tenant        string // "" for a support reply
	CreatedAt     time.Time
	HelpRequestID string
}

// UnmarshalJSON accepts an id that is a number or a string.
func (m *Message) UnmarshalJSON(b []byte) error {
	var in struct {
		ID            json.RawMessage `json:"id"`
		Body          string          `json:"body"`
		Tenant        string          `json:"tenant"`
		CreatedAt     time.Time       `json:"created_at"`
		HelpRequestID json.RawMessage `json:"help_request_id"`
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	m.ID, m.Body, m.Tenant, m.CreatedAt = idString(in.ID), in.Body, in.Tenant, in.CreatedAt
	m.HelpRequestID = idString(in.HelpRequestID)
	return nil
}

// idString reads an id that is a JSON string or number; a number keeps all its digits.
func idString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

// messagePage is the number of messages one GET /api/messages call returns at most (the oldest newer ones).
const messagePage = 50

// Messages calls GET /api/messages?since=since and pages forward until the server has no more (at most 10
// pages). The server returns the oldest 50 newer messages first, so a page that is full is followed by a call
// with since set to its newest time minus a few seconds; the result has each id once, oldest first.
func (c *Client) Messages(ctx context.Context, since time.Time) ([]Message, error) {
	var all []Message
	seen := map[string]bool{}
	for page := 0; page < 10; page++ {
		var raw json.RawMessage
		q := url.Values{"since": {since.UTC().Format(time.RFC3339)}}
		if err := c.do(ctx, http.MethodGet, "/api/messages?"+q.Encode(), nil, &raw); err != nil {
			return all, err
		}
		var list []Message
		if err := json.Unmarshal(raw, &list); err != nil {
			var wrapped struct {
				Messages []Message `json:"messages"`
			}
			if err2 := json.Unmarshal(raw, &wrapped); err2 != nil {
				return all, errors.New("the Labmill account sent a list of messages that Labmill can't read")
			}
			list = wrapped.Messages
		}
		newest, added, withID := since, 0, 0
		for _, m := range list {
			if m.CreatedAt.After(newest) {
				newest = m.CreatedAt
			}
			if m.ID == "" {
				continue
			}
			withID++
			if !seen[m.ID] {
				seen[m.ID] = true
				all = append(all, m)
				added++
			}
		}
		if len(list) > 0 && withID == 0 {
			return all, errors.New("the Labmill account sent messages without an id that Labmill can read")
		}
		if len(list) < messagePage || !newest.After(since) {
			break
		}
		if added == 0 {
			// A full page of ids seen already (the overlap is wider than a page): move on without it.
			since = newest
		} else {
			since = newest.Add(-5 * time.Second)
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].CreatedAt.Before(all[j].CreatedAt) })
	return all, nil
}

// messageState is messages.json next to account.json: where the reader stopped. It holds no secret.
type messageState struct {
	Since time.Time `json:"since"`
	Seen  []string  `json:"seen,omitempty"` // ids shown, newest last; the server's overlap of a few seconds is dropped with it
}

const maxSeen = 200

// MessageReader shows the replies of the research computing team: every Every (2 minutes) it asks the
// account for messages newer than the last one shown and calls Notice once for each. Nothing happens while no
// account is linked. A message shown is never shown again, also after a restart (messages.json).
type MessageReader struct {
	LinkPath string
	Every    time.Duration
	First    time.Duration // wait before the first pass: the window may not be ready for a notice yet
	HTTP     *http.Client
	Notice   func(text string) // must not block
	OnError  func(error)       // a failed pass, once while it repeats
}

// statePath is messages.json next to the link file.
func (r *MessageReader) statePath() string {
	return filepath.Join(filepath.Dir(r.LinkPath), "messages.json")
}

// Run polls until ctx ends.
func (r *MessageReader) Run(ctx context.Context) {
	every := r.Every
	if every <= 0 {
		every = 2 * time.Minute
	}
	last := ""
	if r.First > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.First):
		}
	}
	for {
		_, err := r.Pass(ctx)
		if err != nil && ctx.Err() == nil && err.Error() != last && r.OnError != nil {
			r.OnError(err)
		}
		last = ""
		if err != nil {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// Pass reads once and returns how many new messages it showed.
func (r *MessageReader) Pass(ctx context.Context) (int, error) {
	l, ok, err := Load(r.LinkPath)
	if err != nil || !ok {
		return 0, err
	}
	var st messageState
	if b, err := os.ReadFile(r.statePath()); err == nil {
		_ = json.Unmarshal(b, &st) // a damaged file only means a fresh start from the link time
	}
	since := st.Since
	if since.Before(l.LinkedAt) {
		since = l.LinkedAt
	} else {
		since = since.Add(-5 * time.Second)
	}
	list, err := (&Client{URL: l.URL, Token: l.Token, HTTP: r.HTTP}).Messages(ctx, since)
	shown := map[string]bool{}
	for _, id := range st.Seen {
		shown[id] = true
	}
	n := 0
	for _, m := range list {
		if shown[m.ID] {
			continue
		}
		shown[m.ID] = true
		st.Seen = append(st.Seen, m.ID)
		if m.CreatedAt.After(st.Since) {
			st.Since = m.CreatedAt
		}
		if r.Notice != nil {
			r.Notice(MessageText(m))
		}
		n++
	}
	if len(st.Seen) > maxSeen {
		st.Seen = st.Seen[len(st.Seen)-maxSeen:]
	}
	if n > 0 {
		if b, werr := json.Marshal(st); werr == nil {
			if werr = config.WriteFile(r.statePath(), b); werr != nil && err == nil {
				err = werr
			}
		}
	}
	return n, err
}

// MessageText is the sentence shown for m: the text, cleaned and cut. The server sends no sender name.
func MessageText(m Message) string {
	body := textsafe.Clean(m.Body)
	if r := []rune(body); len(r) > 1500 {
		body = string(r[:1500]) + "..."
	}
	return "Reply from your research computing team:\n" + body
}
