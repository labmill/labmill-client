package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"syscall"
	"time"

	"labmill-client/internal/keepawake"
)

// retryPauses are the waits before each new attempt (so 3 retries). It is a
// variable so tests can use zero.
var retryPauses = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}

// WithRetry runs one whole transfer (an Upload, Download or the S3 forms) with
// the laptop kept awake. If it fails with a transient error (see transient) it
// runs fn again, up to len(retryPauses) more times, after a pause. A new
// attempt skips the files already copied, because rclone leaves files with the
// same size and modification time alone. Downloads are then checked again by
// SHA-256 and uploads by size, as in a first attempt. progress, if not nil, gets
// a notice in Current during each pause: the last progress seen, with only
// Current changed, so byte counts never go down. fn must report through the
// progress function it gets. The notice is sent half way
// through the pause so a rate limited meter (500 ms) shows it.
func WithRetry(ctx context.Context, progress func(Progress), fn func(progress func(Progress)) error) error {
	defer keepawake.Hold()()
	var last Progress
	var wrapped func(Progress)
	if progress != nil {
		wrapped = func(p Progress) { last = p; progress(p) }
	}
	err := fn(wrapped)
	for i := 0; err != nil && i < len(retryPauses) && transient(ctx, err); i++ {
		if !sleepCtx(ctx, retryPauses[i]/2) {
			return err
		}
		if progress != nil {
			n := last
			n.Current = fmt.Sprintf("Connection lost, trying again (%d of %d)", i+1, len(retryPauses))
			progress(n)
		}
		if !sleepCtx(ctx, retryPauses[i]/2) {
			return err
		}
		err = fn(wrapped)
	}
	return err
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// SetRetryPausesForTest replaces the retry pauses and returns a function that
// restores them. Only tests in other packages use it.
func SetRetryPausesForTest(d ...time.Duration) (restore func()) {
	old := retryPauses
	retryPauses = d
	return func() { retryPauses = old }
}

// nonTransient lists error texts that a new attempt can't fix.
var nonTransient = []string{
	"permission denied", "unable to authenticate", "no space left",
	"disk full", "quota exceeded", "file changed since", "access denied",
	"x509", "certificate", "credential", "invalid access key", "signature",
}

// transient reports whether err looks like a lost or slow connection. Sign-in
// rejections, a full disk, a changed file and a cancelled context are not.
// Typed errors decide first; text is matched only on the innermost cause,
// because outer text holds user file names ("upload <path>: ...").
func transient(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return false
	}
	// *url.Error is a net.Error; judge what it wraps (S3 certificate and
	// credential errors arrive this way).
	for ue := (*url.Error)(nil); errors.As(err, &ue); ue = nil {
		err = ue.Err
	}
	leaf := err
	for u := errors.Unwrap(leaf); u != nil; u = errors.Unwrap(leaf) {
		leaf = u
	}
	msg := strings.ToLower(leaf.Error())
	for _, s := range nonTransient {
		if strings.Contains(msg, s) {
			return false
		}
	}
	var ne net.Error
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ETIMEDOUT) || errors.As(err, &ne) {
		return true
	}
	for _, s := range []string{
		"connection reset", "broken pipe", "connection lost", "connection closed",
		"connection refused", "timeout", "timed out", "deadline exceeded", "eof",
		"network is unreachable", "no route to host", "i/o error", "ssh:", "handshake failed",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
