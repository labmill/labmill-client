package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// The setting is cached between renders, refreshed after a few seconds, and
// the marker stays visible while a transfer owns the status line.
func TestAutoApproveCacheAndTransferMarker(t *testing.T) {
	reads := 0
	on := true
	m := newModel(Options{Session: &countSession{}, Status: "c",
		AutoApprove:    func() bool { reads++; return on },
		SetAutoApprove: func(bool) error { return nil }})
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	step(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	reads = 0
	for i := 0; i < 20; i++ {
		m.statusBar()
	}
	if reads > 1 {
		t.Errorf("read the setting %d times for 20 renders", reads)
	}
	on = false
	m.statusBar()
	if !strings.Contains(stripStyles(m.statusBar()), "AUTO-APPROVE ON") {
		t.Error("the cache was bypassed")
	}
	now = now.Add(autoApproveRefresh + time.Second)
	if strings.Contains(stripStyles(m.statusBar()), "AUTO-APPROVE") {
		t.Error("the cache never refreshed")
	}
	on = true
	m.refreshAutoApprove()
	m.xfer = "Transferred 1 MiB"
	if bar := stripStyles(m.statusBar()); !strings.Contains(bar, "Transferred 1 MiB") || !strings.Contains(bar, "AUTO-APPROVE ON") {
		t.Errorf("transfer status line: %q", bar)
	}
}
