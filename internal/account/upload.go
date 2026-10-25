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

// Uploader sends the reports of finished jobs to the linked account. Each
// pass (1) queues a report for every job that ended after the link was made
// and has none yet, and (2) sends the due reports. The queue is the store's
// uploads table, so reports wait while the computer is offline and survive a
// restart; a failed send is tried again later (Backoff). The API stores a
// client_job_ref once, so a report sent twice (two Labmill processes) makes
// one job. With no saved link a pass does nothing: nothing is queued or sent.
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

	mu      sync.Mutex
	systems map[string]string // cluster -> hpc_system_id, for this process
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
	if err := u.enqueue(ctx, l.LinkedAt); err != nil {
		return 0, err
	}
	due, err := u.Store.DueUploads(ctx, u.now(), 50)
	if err != nil {
		return 0, err
	}
	c := &Client{URL: l.URL, Token: l.Token, HTTP: u.HTTP}
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
	return sent, nil
}

// enqueue queues a report for each job that ended at or after since.
func (u *Uploader) enqueue(ctx context.Context, since time.Time) error {
	jobs, err := u.Store.Jobs(ctx, store.JobFilter{
		States:       []store.JobState{store.StateCompleted, store.StateFetched, store.StateFailed, store.StateCanceled},
		UpdatedSince: since,
	})
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.RemoteID == "" {
			continue // canceled before it was submitted
		}
		if has, err := u.Store.HasUpload(ctx, j.ID, j.RemoteID); err != nil || has {
			if err != nil {
				return err
			}
			continue
		}
		evs, err := u.Store.Events(ctx, j.ID, 0)
		if err != nil {
			return err
		}
		ref, err := newRef()
		if err != nil {
			return err
		}
		rep, ok := BuildReport(j, evs, ref)
		if !ok || rep.EndedAt == nil || rep.EndedAt.Before(since) {
			continue // ended before the link was made
		}
		b, err := json.Marshal(rep)
		if err != nil {
			return err
		}
		if _, err := u.Store.AddUpload(ctx, store.Upload{JobID: j.ID, RemoteID: j.RemoteID, Ref: ref,
			Cluster: j.Cluster, Payload: string(b), NextAt: u.now()}); err != nil {
			return err
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
