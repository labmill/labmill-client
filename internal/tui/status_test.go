package tui

import (
	"strings"
	"testing"
)

// A status text that changes after startup reaches the status bar through
// the bridge's StatusLine hook.
func TestStatusLineHookUpdatesStatusBar(t *testing.T) {
	m := newDirect(t, 200, 30)
	m.o.Status = "bigcluster (Slurm)"
	step(t, m, StatusMsg{Text: "bigcluster (Slurm) · 1 startup warning"})
	if bar := stripStyles(m.statusBar()); !strings.Contains(bar, "1 startup warning") {
		t.Errorf("status bar = %q", bar)
	}
}

// The bridge queues the message for the model (and never blocks).
func TestBridgeStatusLineQueuesStatusMsg(t *testing.T) {
	b := NewBridge()
	b.StatusLine("x")
	select {
	case msg := <-b.msgs:
		if sm, ok := msg.(StatusMsg); !ok || sm.Text != "x" {
			t.Errorf("msg = %#v", msg)
		}
	default:
		t.Fatal("no message queued")
	}
	for range cap(b.msgs) + 5 { // a full queue must not block
		b.StatusLine("y")
	}
}
