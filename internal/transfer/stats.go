package transfer

import (
	"fmt"
	"time"
)

// Update is one live transfer notice for a front end: the start (Text is the
// rclone-lib parameter line), throttled progress (Text is the rclone-style
// stats line) and the end (Text is the final summary line, also after a partial
// failure; "" only when nothing was transferred). The numbers are for a progress bar.
type Update struct {
	Tool string // "upload_data" or "fetch_results"
	Kind string // "start", "progress" or "end"
	Text string

	Bytes, TotalBytes int64
	Files, TotalFiles int
	Current           string  // file being processed
	BytesPerSec       float64 // smoothed rate
	ETASeconds        int     // -1 when unknown
}

// Meter turns Progress reports into rclone-style stats, with a smoothed rate
// and ETA, and throttles how often a line is due. Not safe for concurrent use.
type Meter struct {
	// MinInterval is the shortest time between lines (default 500 ms).
	MinInterval time.Duration
	now         func() time.Time

	start, last time.Time
	lastBytes   int64
	rate        float64
	started     bool
}

// NewMeter returns a Meter that starts timing now.
func NewMeter() *Meter { return newMeter(time.Now) }

func newMeter(now func() time.Time) *Meter {
	return &Meter{MinInterval: 500 * time.Millisecond, now: now, start: now()}
}

// Observe records p and returns the Update to show, and whether one is due:
// the first report, then at most one per MinInterval, and always the last
// (all bytes and files done).
func (m *Meter) Observe(p Progress) (Update, bool) {
	now := m.now()
	if dt := now.Sub(m.last); m.started && dt >= 100*time.Millisecond {
		inst := float64(p.Bytes-m.lastBytes) / dt.Seconds()
		if m.rate == 0 {
			m.rate = inst
		} else {
			m.rate = 0.7*m.rate + 0.3*inst
		}
	}
	complete := p.Files >= p.TotalFiles && p.Bytes >= p.TotalBytes && p.TotalFiles > 0
	if m.started && !complete && now.Sub(m.last) < m.MinInterval {
		return Update{}, false
	}
	m.started, m.last, m.lastBytes = true, now, p.Bytes
	return m.update(p), true
}

func (m *Meter) update(p Progress) Update {
	eta := -1
	if m.rate >= 1 && p.TotalBytes > p.Bytes {
		eta = int(min(float64(p.TotalBytes-p.Bytes)/m.rate+0.5, 99*3600))
	} else if p.TotalBytes > 0 && p.Bytes >= p.TotalBytes {
		eta = 0
	}
	u := Update{Kind: "progress", Bytes: p.Bytes, TotalBytes: p.TotalBytes, Files: p.Files, TotalFiles: p.TotalFiles,
		Current: p.Current, BytesPerSec: m.rate, ETASeconds: eta}
	u.Text = FormatStats(u, p.Checking > 0)
	return u
}

// Summary returns the closing line, e.g. "Transferred 4.0 GiB in 1m31s
// (45 MiB/s), 10 files", for the last progress p.
func (m *Meter) Summary(p Progress) string {
	el := m.now().Sub(m.start)
	rate := 0.0
	if el > 0 {
		rate = float64(p.Bytes) / el.Seconds()
	}
	files := "files"
	if p.Files == 1 {
		files = "file"
	}
	return fmt.Sprintf("Transferred %s in %s (%s/s), %d %s", FormatBytes(p.Bytes), formatDuration(int(el.Seconds()+0.5)), FormatBytes(int64(rate)), p.Files, files)
}

// FormatStats formats u like rclone's stats line:
//
//	Transferred: 1.2 GiB / 4.0 GiB, 30%, 45 MiB/s, ETA 1m02s · Files: 3 / 10 · now: data/part-003.csv
//
// An unknown total or ETA shows "-". checking adds "(checking)" to the file
// name: the pre-send re-check of a local file is running.
func FormatStats(u Update, checking bool) string {
	pct := "-"
	if u.TotalBytes > 0 {
		pct = fmt.Sprintf("%d%%", min(u.Bytes, u.TotalBytes)*100/u.TotalBytes)
	}
	eta := "-"
	if u.ETASeconds >= 0 {
		eta = formatDuration(u.ETASeconds)
	}
	s := fmt.Sprintf("Transferred: %s / %s, %s, %s/s, ETA %s · Files: %d / %d",
		FormatBytes(u.Bytes), FormatBytes(u.TotalBytes), pct, FormatBytes(int64(u.BytesPerSec)), eta, u.Files, u.TotalFiles)
	if u.Current != "" {
		s += " · now: " + u.Current
		if checking {
			s += " (checking)"
		}
	}
	return s
}

// FormatBytes formats n with binary units ("512 B", "1.2 GiB").
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", max(n, 0))
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// formatDuration formats seconds as "45s", "1m02s" or "2h03m".
func formatDuration(sec int) string {
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
	}
	return fmt.Sprintf("%dh%02dm", sec/3600, sec%3600/60)
}
