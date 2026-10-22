package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/transfer"
)

// The retry notice must get through the meter's throttle and keep the byte
// counts of the failed attempt.
func TestRetryNoticeReachesFrontEnd(t *testing.T) {
	defer transfer.SetRetryPausesForTest(1200*time.Millisecond, 0, 0)()
	var got []transfer.Update
	ts := &toolset{env: &Env{Transfer: func(u transfer.Update) { got = append(got, u) }}}
	cb, finish := ts.startTransfer("upload_data", 1, "rclone copy")
	defer finish()
	calls := 0
	err := transfer.WithRetry(context.Background(), cb, func(pr func(transfer.Progress)) error {
		calls++
		if calls == 1 {
			pr(transfer.Progress{Bytes: 100, TotalBytes: 200, Files: 1, TotalFiles: 2})
			return errors.New("connection reset by peer")
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	for _, u := range got {
		if strings.Contains(u.Current, "trying again (1 of 3)") {
			if u.Bytes != 100 || u.TotalBytes != 200 || u.BytesPerSec < 0 {
				t.Fatalf("notice lost the counts: %+v", u)
			}
			return
		}
	}
	t.Fatalf("notice never reached the front end: %+v", got)
}
