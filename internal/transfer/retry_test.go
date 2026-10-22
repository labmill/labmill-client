package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"syscall"
	"testing"
	"time"
)

func TestTransient(t *testing.T) {
	ctx := context.Background()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"reset", ctx, errors.New("read tcp: connection reset by peer"), true},
		{"eof", ctx, io.ErrUnexpectedEOF, true},
		{"timeout", ctx, errors.New("i/o timeout"), true},
		{"ssh lost", ctx, errors.New("ssh: connection lost"), true},
		{"file name timeout", ctx, fmt.Errorf("upload timeout_scan.csv: %w", errors.New("permission denied")), false},
		{"file name auth", ctx, fmt.Errorf("upload authentication.log: %w", io.ErrUnexpectedEOF), true},
		{"x509", ctx, &url.Error{Op: "Get", URL: "https://s3", Err: errors.New("x509: certificate signed by unknown authority")}, false},
		{"url reset", ctx, &url.Error{Op: "Get", URL: "https://s3", Err: syscall.ECONNRESET}, true},
		{"auth", ctx, errors.New("ssh: handshake failed: ssh: unable to authenticate"), false},
		{"disk", ctx, errors.New("write x: no space left on device"), false},
		{"changed", ctx, errors.New("file changed since you approved the upload: a"), false},
		{"cancelled", cancelled, errors.New("connection reset"), false},
		{"nil", ctx, nil, false},
	} {
		if got := transient(tc.ctx, tc.err); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWithRetry(t *testing.T) {
	old := retryPauses
	retryPauses = []time.Duration{0, 0, 0}
	defer func() { retryPauses = old }()
	lost := errors.New("connection reset by peer")

	calls, notices := 0, []Progress{}
	prog := func(p Progress) { notices = append(notices, p) }
	err := WithRetry(context.Background(), prog, func(pr func(Progress)) error {
		calls++
		pr(Progress{Bytes: 100, TotalBytes: 200, Files: 1, TotalFiles: 2})
		if calls < 3 {
			return lost
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("err=%v calls=%d, want nil 3", err, calls)
	}
	if len(notices) != 5 || notices[3].Current != "Connection lost, trying again (2 of 3)" || notices[3].Bytes != 100 || notices[3].TotalBytes != 200 {
		t.Fatalf("notices = %+v", notices)
	}

	calls = 0
	err = WithRetry(context.Background(), nil, func(func(Progress)) error { calls++; return lost })
	if err == nil || calls != 4 {
		t.Fatalf("always failing: err=%v calls=%d, want error 4", err, calls)
	}

	calls = 0
	err = WithRetry(context.Background(), nil, func(func(Progress)) error { calls++; return errors.New("permission denied") })
	if err == nil || calls != 1 {
		t.Fatalf("permanent: err=%v calls=%d, want error 1", err, calls)
	}
}
