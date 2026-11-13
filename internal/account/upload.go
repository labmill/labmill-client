package account

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"labmill-client/internal/store"
)

// Uploader sends job reports and problem reports to the linked account. Each
// pass (1) queues a report for every submitted job that changed after the
// link was made (waiting, running or ended; a job that ended before the link
// is left out) and queues it again when it changed, (2) sends the due
// reports, and (3) sends the due problem reports. The queues are the store's
// uploads and client_events tables, so reports wait while the computer is
// offline and survive a restart; a failed send is tried again later
// (Backoff). One job keeps one client_job_ref, so the API updates one job.
// A private job and a fully private account (GET /api/me contributes false)
// send only the scheduler's facts and no problem reports. With no saved
// link a pass does nothing: nothing is queued or sent.
type Uploader struct {
	Store    *store.Store
	LinkPath string
	// Institution names the institution of a cluster (the site profile's
	// name), to find its HPC system in the API. nil or "" matches by the
	// cluster name alone when that is unique.
	Institution func(cluster string) string
	// Ref gives the login host and site domain of a cluster for
	// GET /api/systems/resolve. nil: the cluster name alone.
	Ref   func(cluster string) SystemRef
	HTTP  *http.Client     // nil: default
	Now   func() time.Time // nil: time.Now
	Every time.Duration    // 0: 60 s
	// OnError, if set, gets a failed pass's error in Run (once while it repeats).
	OnError func(error)
	// Words gives the names only this computer knows for a job (user and
	// account names, host names, the job's folders), redacted from what is
	// sent. nil: the job's name only.
	Words func(j store.Job) []string

	mu        sync.Mutex
	systems   map[string]string // cluster -> hpc_system_id, for this process
	contrib   bool              // the account contributes (GET /api/me), as of contribAt
	contribAt time.Time
}

// contribTTL is how long the answer of GET /api/me about contribution is used.
const contribTTL = time.Hour

// contributes asks the API whether the account contributes, at most once per
// contribTTL. An error counts as false for this pass (facts only).
func (u *Uploader) contributes(ctx context.Context, c *Client) bool {
	u.mu.Lock()
	if !u.contribAt.IsZero() && u.now().Sub(u.contribAt) < contribTTL {
		defer u.mu.Unlock()
		return u.contrib
	}
	u.mu.Unlock()
	me, err := c.Whoami(ctx)
	if err != nil {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.contrib, u.contribAt = me.Contributes != nil && *me.Contributes, u.now()
	return u.contrib
}

// redactor is the Redactor for job j.
func (u *Uploader) redactor(j store.Job) Redactor {
	words := []string{j.Name}
	if u.Words != nil {
		words = append(words, u.Words(j)...)
	}
	return NewRedactor(words...)
}

// Backoff is the wait after the n-th failed send (n >= 1): 1 min doubled, at most 6 h.
func Backoff(n int) time.Duration {
	d := time.Minute
	for i := 1; i < n && d < 6*time.Hour; i++ {
		d *= 2
	}
	return min(d, 6*time.Hour)
}

func (u *Uploader) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

// Run makes a pass every Every until ctx ends.
func (u *Uploader) Run(ctx context.Context) {
	every := u.Every
	if every <= 0 {
		every = time.Minute
	}
	last := ""
	for {
		_, err := u.Pass(ctx)
		if err != nil && ctx.Err() == nil && err.Error() != last && u.OnError != nil {
			u.OnError(err)
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

// Pass queues and sends once. It returns how many reports were sent.
func (u *Uploader) Pass(ctx context.Context) (sent int, err error) {
	l, ok, err := Load(u.LinkPath)
	if err != nil || !ok {
		return 0, err
	}
	c := &Client{URL: l.URL, Token: l.Token, HTTP: u.HTTP}
	full := u.contributes(ctx, c)
	if err := u.enqueue(ctx, l.LinkedAt, full); err != nil {
		return 0, err
	}
	due, err := u.Store.DueUploads(ctx, u.now(), 50)
	if err != nil {
		return 0, err
	}
	for _, up := range due {
		err := u.send(ctx, c, up)
		if err == nil {
			sent++
			if err := u.Store.UploadSent(ctx, up.JobID, up.RemoteID, u.now()); err != nil {
				return sent, err
			}
			continue
		}
		if ctx.Err() != nil {
			return sent, ctx.Err()
		}
		next := u.now().Add(Backoff(up.Attempts + 1))
		var se *StatusError
		if errors.As(err, &se) && se.Code == http.StatusBadRequest {
			next = store.Never // the API refuses this report; sending it again won't help
		}
		if err := u.Store.UploadFailed(ctx, up.JobID, up.RemoteID, next, err.Error()); err != nil {
			return sent, err
		}
		if errors.Is(err, ErrRejected) {
			return sent, err // the token is no longer valid: the other reports wait too
		}
	}
	return sent, u.sendEvents(ctx, c, full)
}

// maxReportBytes keeps a report below the API's 1 MB body limit.
const maxReportBytes = 900 << 10

// lastHalf is the last half of l (nil when l has one line or none).
func lastHalf(l []string) []string {
	if len(l) <= 1 {
		return nil
	}
	return l[len(l)/2:]
}

// reportWindow: only jobs changed in this time are looked at again in a pass
// (a job's report changes when its state or output changes, both right away).
const reportWindow = 14 * 24 * time.Hour

// enqueue queues or updates the report of each submitted job changed at or
// after since (and in the last reportWindow). An ended job counts only when
// it ended at or after since. full adds the script and the output lines.
func (u *Uploader) enqueue(ctx context.Context, since time.Time, full bool) error {
	from := since
	if w := u.now().Add(-reportWindow); w.After(from) {
		from = w
	}
	jobs, err := u.Store.Jobs(ctx, store.JobFilter{UpdatedSince: from, States: []store.JobState{store.StateSubmitted,
		store.StatePending, store.StateRunning, store.StateCompleted, store.StateFetched, store.StateFailed, store.StateCanceled}})
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.RemoteID == "" {
			continue // canceled before it was submitted
		}
		old, has, err := u.Store.UploadOf(ctx, j.ID, j.RemoteID)
		if err != nil {
			return err
		}
		if has && old.NextAt.Equal(store.Never) {
			continue // the API refused this report
		}
		evs, err := u.Store.Events(ctx, j.ID, 0)
		if err != nil {
			return err
		}
		ref := old.Ref
		if !has {
			if ref, err = newRef(); err != nil {
				return err
			}
		}
		rep, ok := BuildReport(j, evs, ref)
		if !ok || (rep.EndedAt != nil && rep.EndedAt.Before(since)) {
			continue // ended before the link was made
		}
		if full && rep.Shared == nil {
			AddDetails(&rep, j, evs, u.redactor(j))
		}
		b, err := json.Marshal(rep)
		// The API takes at most 1 MB; JSON escapes "<" of the redaction marks
		// as 6 bytes, so very marked-up output can grow past it: keep fewer lines.
		for err == nil && len(b) > maxReportBytes && len(rep.StdoutLines)+len(rep.StderrLines) > 0 {
			rep.StdoutLines, rep.StderrLines = lastHalf(rep.StdoutLines), lastHalf(rep.StderrLines)
			b, err = json.Marshal(rep)
		}
		if err != nil {
			return err
		}
		switch {
		case has && old.Payload == string(b):
		case has:
			if err := u.Store.ReplaceUpload(ctx, j.ID, j.RemoteID, string(b), u.now()); err != nil {
				return err
			}
		default:
			if _, err := u.Store.AddUpload(ctx, store.Upload{JobID: j.ID, RemoteID: j.RemoteID, Ref: ref,
				Cluster: j.Cluster, Payload: string(b), NextAt: u.now()}); err != nil {
				return err
			}
		}
	}
	return nil
}

func newRef() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "lmc-" + hex.EncodeToString(b), nil
}

// send adds the HPC system to a queued report and sends it.
func (u *Uploader) send(ctx context.Context, c *Client, up store.Upload) error {
	var rep JobIn
	if err := json.Unmarshal([]byte(up.Payload), &rep); err != nil {
		return &StatusError{Code: http.StatusBadRequest, Text: "damaged queued report"}
	}
	id, err := u.system(ctx, c, up.Cluster)
	if err != nil {
		return err
	}
	rep.HPCSystemID = id
	return c.SendJob(ctx, rep)
}

// system finds the API's id of cluster: GET /api/systems/resolve (login
// host, name, site domain); when that has no single answer (400, 404, 409,
// or an older server), the listed system with the same name (and the same
// institution when Institution names one).
func (u *Uploader) system(ctx context.Context, c *Client, cluster string) (string, error) {
	u.mu.Lock()
	id, ok := u.systems[cluster]
	u.mu.Unlock()
	if ok {
		return id, nil
	}
	ref := SystemRef{Name: cluster}
	if u.Ref != nil {
		ref = u.Ref(cluster)
	}
	s, err := c.Resolve(ctx, ref)
	var se *StatusError
	if err == nil && s.ID != "" {
		u.remember(cluster, s.ID)
		return s.ID, nil
	}
	// 400 too: a host the server refuses must not stop the report (send treats 400 as final).
	if err != nil && !(errors.As(err, &se) && (se.Code == http.StatusBadRequest || se.Code == http.StatusNotFound || se.Code == http.StatusConflict)) {
		return "", err
	}
	list, err := c.Systems(ctx, cluster)
	if err != nil {
		return "", err
	}
	inst := ""
	if u.Institution != nil {
		inst = u.Institution(cluster)
	}
	var match []string
	for _, s := range list {
		if strings.EqualFold(s.Name, cluster) && (inst == "" || strings.EqualFold(s.Institution, inst)) {
			match = append(match, s.ID)
		}
	}
	if len(match) != 1 {
		return "", fmt.Errorf("the Labmill account doesn't know the cluster %s yet (%d matches)", cluster, len(match))
	}
	u.remember(cluster, match[0])
	return match[0], nil
}

func (u *Uploader) remember(cluster, id string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.systems == nil {
		u.systems = map[string]string{}
	}
	u.systems[cluster] = id
}

// Queue counts the reports: sent, waiting, and refused by the API.
func Queue(ctx context.Context, st *store.Store) (sent, waiting, refused int, lastErr string, err error) {
	ups, err := st.Uploads(ctx)
	if err != nil {
		return 0, 0, 0, "", err
	}
	for _, up := range ups {
		switch {
		case !up.SentAt.IsZero():
			sent++
		case up.NextAt.Equal(store.Never):
			refused++
		default:
			waiting++
			if up.LastError != "" {
				lastErr = up.LastError
			}
		}
	}
	return sent, waiting, refused, lastErr, nil
}

// sendEvents sends the due problem reports in one batch. Without
// contribution (full false), the due reports are dropped unsent.
func (u *Uploader) sendEvents(ctx context.Context, c *Client, full bool) error {
	due, err := u.Store.DueClientEvents(ctx, u.now(), maxEventsPerCall)
	if err != nil || len(due) == 0 {
		return err
	}
	var batch []ClientEvent
	var ids, drop []int64
	for _, d := range due {
		var e ClientEvent
		if !full || json.Unmarshal([]byte(d.Payload), &e) != nil {
			drop = append(drop, d.ID)
			continue
		}
		if d.Cluster != "" {
			e.HPCSystemID, _ = u.system(ctx, c, d.Cluster) // unknown: sent without it
		}
		if d.JobID != 0 {
			j, err := u.Store.Job(ctx, d.JobID)
			if err == nil && j.RemoteID != "" {
				evs, _ := u.Store.Events(ctx, j.ID, 0)
				if Private(evs) && e.Kind != KindWorked {
					drop = append(drop, d.ID)
					continue
				}
				if up, ok, _ := u.Store.UploadOf(ctx, j.ID, j.RemoteID); ok {
					e.ClientJobRef = up.Ref
				}
			}
		}
		if e.Kind == KindWorked && e.ClientJobRef == "" {
			drop = append(drop, d.ID) // the API needs the job reference
			continue
		}
		batch = append(batch, e)
		ids = append(ids, d.ID)
	}
	if err := u.Store.ClientEventsDone(ctx, drop, false, store.Never); err != nil {
		return err
	}
	if len(batch) == 0 {
		return nil
	}
	err = c.SendEvents(ctx, batch)
	var se *StatusError
	switch {
	case err == nil:
		return u.Store.ClientEventsDone(ctx, ids, true, u.now())
	case errors.As(err, &se) && se.Code == http.StatusBadRequest:
		return u.Store.ClientEventsDone(ctx, ids, false, store.Never)
	case ctx.Err() != nil:
		return ctx.Err()
	}
	if derr := u.Store.ClientEventsDone(ctx, ids, false, u.now().Add(Backoff(due[0].Attempts+1))); derr != nil {
		return derr
	}
	return err
}
