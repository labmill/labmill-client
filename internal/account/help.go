package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"labmill-client/internal/store"
)

// Tenant is one entry of the tenants of GET /api/me: a team the person belongs to,
// for example the research computing team of an institution.
type Tenant struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// MaxHelpReport is the largest report POST /api/help takes.
const MaxHelpReport = 16 << 10

// HelpIn is the body of POST /api/help. Report is a JSON object of at most
// MaxHelpReport bytes: the failure facts the user chose to send.
type HelpIn struct {
	Tenant string          `json:"tenant"`
	Body   string          `json:"body"`
	Report json.RawMessage `json:"report,omitempty"`
}

// Help calls POST /api/help and returns the id of the request (202 {id}).
func (c *Client) Help(ctx context.Context, in HelpIn) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, "/api/help", in, &out)
	return out.ID, err
}

// HelpReport makes the report that goes with a request for help about job
// j: the scheduler's facts, and with details also the redacted script and
// output lines. The script, then the output lines, are cut until the JSON fits MaxHelpReport.
// It returns the report as indented JSON, the form the user approves.
func HelpReport(j store.Job, evs []store.Event, ref string, red Redactor, details bool) (json.RawMessage, error) {
	in, ok := BuildReport(j, evs, ref)
	if !ok {
		return nil, errors.New("the job was not submitted, so it has no scheduler facts")
	}
	if details {
		AddDetails(&in, j, evs, red)
	}
	for {
		b, err := json.MarshalIndent(in, "", "  ")
		if err != nil {
			return nil, err
		}
		if len(b) <= MaxHelpReport {
			return b, nil
		}
		switch {
		case in.Script != "":
			in.Script = ""
		case len(in.StdoutLines) > 1 || len(in.StderrLines) > 1:
			in.StdoutLines, in.StderrLines = in.StdoutLines[len(in.StdoutLines)/2:], in.StderrLines[len(in.StderrLines)/2:]
		case len(in.StdoutLines) > 0 || len(in.StderrLines) > 0:
			in.StdoutLines, in.StderrLines = nil, nil
		default:
			return nil, errors.New("the report is too large")
		}
	}
}

// PickTenant returns the tenant named by team (id or name, case-insensitive;
// "" when there is only one). The error lists the names when the choice is open.
func PickTenant(ts []Tenant, team string) (Tenant, error) {
	team = strings.TrimSpace(team)
	switch {
	case len(ts) == 0:
		return Tenant{}, errors.New("your Labmill account belongs to no research computing team yet")
	case team == "" && len(ts) == 1:
		return ts[0], nil
	}
	for _, t := range ts {
		if team != "" && (t.ID == team || strings.EqualFold(t.Name, team)) {
			return t, nil
		}
	}
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = t.Name
	}
	return Tenant{}, errors.New("which team? one of: " + strings.Join(names, ", "))
}
