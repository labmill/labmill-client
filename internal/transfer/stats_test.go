package transfer

import (
	"testing"
	"time"
)

func TestFormatStats(t *testing.T) {
	const gi = 1 << 30
	tests := []struct {
		name     string
		u        Update
		checking bool
		want     string
	}{
		{"typical", Update{Bytes: 1288490200, TotalBytes: 4 * gi, BytesPerSec: 45 << 20, ETASeconds: 62, Files: 3, TotalFiles: 10, Current: "data/part-003.csv"},
			false, "Transferred: 1.2 GiB / 4.0 GiB, 30%, 45.0 MiB/s, ETA 1m02s · Files: 3 / 10 · now: data/part-003.csv"},
		{"unknown total and eta", Update{ETASeconds: -1, TotalFiles: 2}, false,
			"Transferred: 0 B / 0 B, -, 0 B/s, ETA - · Files: 0 / 2"},
		{"small and hours", Update{Bytes: 512, TotalBytes: 1024, ETASeconds: 7384, Files: 1, TotalFiles: 1, Current: "a"}, true,
			"Transferred: 512 B / 1.0 KiB, 50%, 0 B/s, ETA 2h03m · Files: 1 / 1 · now: a (checking)"},
		{"seconds", Update{Bytes: 1, TotalBytes: 2, ETASeconds: 5, TotalFiles: 1}, false,
			"Transferred: 1 B / 2 B, 50%, 0 B/s, ETA 5s · Files: 0 / 1"},
	}
	for _, tt := range tests {
		if got := FormatStats(tt.u, tt.checking); got != tt.want {
			t.Errorf("%s:\n got %s\nwant %s", tt.name, got, tt.want)
		}
	}
}

func TestMeterThrottleAndSummary(t *testing.T) {
	now := time.Unix(1000, 0)
	m := newMeter(func() time.Time { return now })
	total := Progress{TotalBytes: 100 << 20, TotalFiles: 2}
	step := func(d time.Duration, bytes int64, files int) (Update, bool) {
		now = now.Add(d)
		p := total
		p.Bytes, p.Files = bytes, files
		return m.Observe(p)
	}
	if _, ok := step(0, 0, 0); !ok {
		t.Fatal("first report must be shown")
	}
	if _, ok := step(100*time.Millisecond, 1<<20, 0); ok {
		t.Error("report after 100 ms shown; want throttled")
	}
	u, ok := step(500*time.Millisecond, 10<<20, 0)
	if !ok {
		t.Fatal("report after 600 ms throttled")
	}
	if u.BytesPerSec <= 0 || u.ETASeconds <= 0 || u.TotalBytes != 100<<20 {
		t.Errorf("update = %+v", u)
	}
	if _, ok := step(10*time.Millisecond, 100<<20, 2); !ok {
		t.Error("the final report must always be shown")
	}
	now = now.Add(88*time.Second + 400*time.Millisecond)
	p := total
	p.Bytes, p.Files = 100<<20, 2
	if got, want := m.Summary(p), "Transferred 100.0 MiB in 1m29s (1.1 MiB/s), 2 files"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestMeterStallAndSpike(t *testing.T) {
	now := time.Unix(1000, 0)
	m := newMeter(func() time.Time { return now })
	total := Progress{TotalBytes: 1 << 40, TotalFiles: 1}
	obs := func(d time.Duration, b int64) Update {
		now = now.Add(d)
		p := total
		p.Bytes = b
		u, _ := m.Observe(p)
		return u
	}
	obs(0, 0)
	if u := obs(time.Second, 0); u.ETASeconds != -1 {
		t.Errorf("stall: ETA = %d, want -1", u.ETASeconds)
	}
	if u := obs(time.Second, 10); u.ETASeconds != 99*3600 {
		t.Errorf("slow: ETA = %d, want the 99 h cap", u.ETASeconds)
	}
	// A report 10 ms after the last shown one must not change the rate.
	before := m.rate
	obs(10*time.Millisecond, 1<<30)
	if m.rate != before {
		t.Errorf("rate changed from %v to %v on a 10 ms interval", before, m.rate)
	}
}
