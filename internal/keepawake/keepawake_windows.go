//go:build windows

package keepawake

import (
	"runtime"

	"golang.org/x/sys/windows"
)

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001
)

// osStart sets the state on a goroutine pinned to one OS thread, because
// SetThreadExecutionState is per thread. The returned function clears it.
func osStart() func() {
	quit, done := make(chan struct{}), make(chan struct{})
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("SetThreadExecutionState")
	go func() {
		runtime.LockOSThread()
		defer close(done)
		proc.Call(uintptr(esContinuous | esSystemRequired))
		<-quit
		proc.Call(uintptr(esContinuous))
	}()
	return func() { close(quit); <-done }
}
