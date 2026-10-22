//go:build !windows

package sshx

import (
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/term"
)

// prepareConsole is a no-op on Unix terminals.
func prepareConsole(in, out *os.File) (restore func()) { return func() {} }

func notifyTermination(c chan<- os.Signal) { signal.Notify(c, syscall.SIGTERM, syscall.SIGHUP) }

// newInputReader reads the terminal f but gives up once done is closed, so
// a goroutine waiting for a key press cannot swallow the first key typed
// after the shell ended (the TUI reads the same input next).
func newInputReader(f *os.File, done <-chan struct{}) io.Reader { return &pollReader{f, done} }

type pollReader struct {
	f    *os.File
	done <-chan struct{}
}

func (r *pollReader) Read(p []byte) (int, error) {
	for {
		select {
		case <-r.done:
			return 0, io.EOF
		default:
		}
		ready, err := waitReadable(r.f, 100*time.Millisecond)
		if err != nil {
			return 0, err
		}
		if !ready {
			continue
		}
		// done may have closed while waiting: leave the input alone.
		select {
		case <-r.done:
			return 0, io.EOF
		default:
		}
		return r.f.Read(p)
	}
}

// watchResize delivers the terminal size (width, height) on every SIGWINCH
// until done is closed.
func watchResize(done <-chan struct{}, fd int) <-chan [2]int {
	out := make(chan [2]int, 1)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(sig)
		for {
			select {
			case <-done:
				return
			case <-sig:
				if w, h, err := term.GetSize(fd); err == nil {
					select {
					case out <- [2]int{w, h}:
					default:
					}
				}
			}
		}
	}()
	return out
}
