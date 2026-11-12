package account

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

// ClientEvent is one problem report in the schema of the API's
// POST /api/client-events. The message is redacted (Redactor.Message).
type ClientEvent struct {
	At            time.Time `json:"at"`
	ClientVersion string    `json:"client_version"`
	OS            string    `json:"os"`
	HPCSystemID   string    `json:"hpc_system_id,omitempty"`
	ClientJobRef  string    `json:"client_job_ref,omitempty"`
	SessionID     string    `json:"session_id"`
	Kind          string    `json:"kind"`
	Tool          string    `json:"tool,omitempty"`
	Message       string    `json:"message"`
}

// The kinds of problem the API knows.
const (
	KindConnection = "connection" // SSH or network
	KindAuth       = "auth"       // a sign-in was refused
	KindModel      = "model"      // the model, the broker or the relay failed
	KindTool       = "tool"       // a tool call failed
	KindSbatch     = "sbatch"     // the scheduler refused a submission
	KindTransfer   = "transfer"   // an upload or download failed
	KindCrash      = "crash"      // a recovered panic
	KindCancel     = "cancel"     // the user stopped a turn
)

const (
	maxEventsPerCall    = 50  // one POST
	maxEventsPerSession = 500 // a loop of failures must not fill the queue
)

// Problems records the problems of one session in the store's queue; the
// Uploader sends them. Nothing is recorded while no account is linked or for
// a private job. Record never blocks on the network and never fails the caller.
type Problems struct {
	Store    *store.Store
	LinkPath string
	Version  string // the client's version
	OS       string // runtime.GOOS
	// Words gives the names only this computer knows (see Uploader.Words);
	// j is the zero Job for a problem outside a job.
	Words func(j store.Job) []string
	Now   func() time.Time // nil: time.Now

	once    sync.Once
	session string
	mu      sync.Mutex
	n       int
}

// Session is this session's random id ("lms-" and 16 hex digits).
func (p *Problems) Session() string {
	p.once.Do(func() {
		b := make([]byte, 8)
		rand.Read(b)
		p.session = "lms-" + hex.EncodeToString(b)
	})
	return p.session
}

// Record queues one problem. tool, cluster and jobID may be empty or 0.
func (p *Problems) Record(kind, tool, cluster string, jobID int64, msg string) {
	p.record(kind, tool, cluster, jobID, msg)
}

// KindWorked is the user's one-tap answer to "did it work?" after a job that
// ran new code. The message is exactly "yes" or "no", and the API needs the
// job reference. It is not a problem: the API leaves it out of the problem list.
const KindWorked = "worked"

// Worked queues the answer for job jobID. Unlike a problem it is sent for a
// private job too (a yes or no carries nothing of the job); a fully private
// account still sends nothing, as for every client event.
func (p *Problems) Worked(jobID int64, yes bool) {
	msg := "no"
	if yes {
		msg = "yes"
	}
	p.record(KindWorked, "", "", jobID, msg)
}

func (p *Problems) record(kind, tool, cluster string, jobID int64, msg string) {
	if p == nil || p.Store == nil || strings.TrimSpace(msg) == "" {
		return
	}
	if _, ok, err := Load(p.LinkPath); err != nil || !ok {
		return
	}
	p.mu.Lock()
	if p.n >= maxEventsPerSession {
		p.mu.Unlock()
		return
	}
	p.n++
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var j store.Job
	if jobID != 0 {
		var err error
		if j, err = p.Store.Job(ctx, jobID); err != nil {
			jobID = 0
		} else if evs, err := p.Store.Events(ctx, jobID, 0); err != nil || (Private(evs) && kind != KindWorked) {
			return
		}
	}
	words := []string{j.Name}
	if p.Words != nil {
		words = append(words, p.Words(j)...)
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	if !toolNameRE.MatchString(tool) {
		tool = ""
	}
	e := ClientEvent{At: now().UTC(), ClientVersion: clientVersion(p.Version), OS: p.OS, SessionID: p.Session(),
		Kind: kind, Tool: tool, Message: msg}
	if kind != KindWorked {
		e.Message = NewRedactor(words...).Message(msg)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	_, _ = p.Store.AddClientEvent(ctx, store.ClientEvent{JobID: jobID, Cluster: cluster, Payload: string(b), NextAt: now()})
}

// clientVersion is v in the form the API accepts ([A-Za-z0-9._+-], at most 64).
func clientVersion(v string) string {
	if versionRE.MatchString(v) {
		return v
	}
	return "unknown"
}

// ToolKind is the kind of a failed tool call: a sign-in or connection
// problem by its text, else by the tool.
func ToolKind(tool, text string) string {
	t := strings.ToLower(text)
	for _, s := range []string{"unable to authenticate", "authentication failed", "permission denied (publickey",
		"permission denied (keyboard", "login failed", "sign-in failed", "sign in failed", "key was not accepted"} {
		if strings.Contains(t, s) {
			return KindAuth
		}
	}
	for _, s := range []string{"connection refused", "connection reset", "no route to host", "network is unreachable",
		"i/o timeout", "dial tcp", "handshake failed", "can't connect", "could not connect", "connection lost",
		"no such host", "broken pipe"} {
		if strings.Contains(t, s) {
			return KindConnection
		}
	}
	switch tool {
	case "submit_job":
		return KindSbatch
	case "upload_data", "fetch_results", "stage_data", "s3_credentials_to_cluster":
		return KindTransfer
	}
	return KindTool
}

// CollectOutput saves the redacted first and last lines of the output of
// job j's current run as an EventOutput event, once per run, when j has
// ended, is not private and be can read output files. A failed job keeps
// its first 100 lines too; every job its last 500 lines of stdout and stderr.
func CollectOutput(ctx context.Context, st *store.Store, be backend.Backend, j store.Job, red Redactor) error {
	or, ok := be.(backend.OutputReader)
	if !ok || !Final(j.State) || j.RemoteID == "" {
		return nil
	}
	evs, err := st.Events(ctx, j.ID, 0)
	if err != nil {
		return err
	}
	if Private(evs) {
		return nil
	}
	for _, e := range evs {
		var o Output
		if e.Kind == EventOutput && json.Unmarshal([]byte(e.Text), &o) == nil && o.RemoteID == j.RemoteID {
			return nil
		}
	}
	head := 0
	if j.State == store.StateFailed {
		head = 100
	}
	stdout, stderr, err := or.Output(ctx, j.RemoteID, head, 500)
	if err != nil {
		return err
	}
	b, err := json.Marshal(Output{RemoteID: j.RemoteID, Stdout: red.Lines(stdout), Stderr: red.Lines(stderr)})
	if err != nil {
		return err
	}
	return st.AddEvent(ctx, j.ID, EventOutput, string(b))
}
