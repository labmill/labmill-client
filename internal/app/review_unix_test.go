//go:build !windows

package app

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO inside the allowed folders must be refused, not opened (an open
// would block forever).
func TestPreviewRefusesFIFO(t *testing.T) {
	h := newHarness(t)
	fifo := filepath.Join(h.home, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skip("mkfifo unavailable:", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := h.run(t, "preview_file", map[string]any{"path": fifo})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("preview of a FIFO succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("preview of a FIFO hung")
	}
}
