// Package stall runs a blocking file operation (open, read, stat) so that a
// stalled one cannot hang its caller.
//
// A read on a network mount that stopped answering, or on a cloud
// placeholder file ("files on demand" of OneDrive, Dropbox and the like), can
// block inside the kernel for minutes, and no context reaches it. Run does the
// operation in a goroutine and stops waiting for it when the limit or the
// context expires. The goroutine can't be cancelled: an abandoned operation
// may finish later (its result is then dropped, and the operation itself must
// close what it opened). To keep abandoned operations from piling up, a
// Limiter allows only a fixed number to be in flight at once, abandoned ones
// included; when all slots are taken by stuck reads, new operations are
// refused at once with an error instead of starting yet another goroutine.
package stall

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefaultSlots is the number of operations the Default limiter lets run (and
// stay stuck) at the same time.
const DefaultSlots = 32

// Limiter bounds the operations in flight, abandoned ones included.
type Limiter struct{ slots chan struct{} }

// NewLimiter returns a Limiter allowing n operations at once (at least 1).
func NewLimiter(n int) *Limiter {
	return &Limiter{slots: make(chan struct{}, max(n, 1))}
}

// Default is the limiter the file tools share.
var Default = NewLimiter(DefaultSlots)

// Error says a file didn't answer: the user-facing note is its message.
type Error struct {
	What  string        // what was being read (a file name or path)
	Limit time.Duration // how long it was given
	Busy  bool          // refused because earlier stuck reads fill every slot
}

func (e *Error) Error() string {
	if e.Busy {
		return fmt.Sprintf("skipped: %s (a network drive or cloud folder stopped answering; reconnect it, then quit Labmill from the tray (or the terminal) and start it again)", e.What)
	}
	return fmt.Sprintf("skipped: %s didn't answer within %d s", e.What, int((e.Limit+time.Second-1)/time.Second))
}

// IsStall reports whether err is (or wraps) a stall Error.
func IsStall(err error) bool { var e *Error; return errors.As(err, &e) }

// Run calls fn in a goroutine and returns its result. If fn hasn't returned
// after limit (0 means no limit besides ctx), or ctx ends first, Run returns
// an *Error (ctx.Err() when ctx ended first, or was already done) and leaves
// fn running; its eventual result is dropped. what names
// the file for the message. Run returns an *Error with Busy set, without
// starting fn, when the limiter has no free slot.
func Run[T any](ctx context.Context, l *Limiter, what string, limit time.Duration, fn func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	select {
	case l.slots <- struct{}{}:
	default:
		return zero, &Error{What: what, Limit: limit, Busy: true}
	}
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { <-l.slots }()
		v, err := fn()
		done <- result{v, err}
	}()
	var timer <-chan time.Time
	if limit > 0 {
		t := time.NewTimer(limit)
		defer t.Stop()
		timer = t.C
	}
	select {
	case r := <-done:
		return r.v, r.err
	case <-timer:
	case <-ctx.Done():
		select {
		case r := <-done:
			return r.v, r.err
		default:
		}
		return zero, ctx.Err()
	}
	// A result that arrived at the same moment still counts.
	select {
	case r := <-done:
		return r.v, r.err
	default:
	}
	return zero, &Error{What: what, Limit: limit}
}

// Stuck returns the number of operations running or abandoned right now (a
// count that stays above zero for long means reads that never return).
func (l *Limiter) Stuck() int { return len(l.slots) }

// WaitIdle waits up to d for every running or abandoned operation to finish
// and reports whether they did. It synchronizes with their ends (tests use it
// before restoring what the operations read).
func (l *Limiter) WaitIdle(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	n := 0
	defer func() {
		for ; n > 0; n-- {
			<-l.slots
		}
	}()
	for n < cap(l.slots) {
		select {
		case l.slots <- struct{}{}:
			n++
		case <-timer.C:
			return false
		}
	}
	return true
}
