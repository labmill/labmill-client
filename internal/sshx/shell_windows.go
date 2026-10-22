//go:build windows

package sshx

import (
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

// prepareConsole turns on virtual-terminal processing for the output (so the
// remote shell's escape sequences work, with newline auto-return off so the
// cursor behaves as in other terminals) and virtual-terminal input for the
// input (arrow keys arrive as escape sequences); restore puts both back.
func prepareConsole(in, out *os.File) (restore func()) {
	var restores []func()
	set := func(f *os.File, flag uint32) {
		h := windows.Handle(f.Fd())
		var mode uint32
		if windows.GetConsoleMode(h, &mode) != nil {
			return
		}
		if windows.SetConsoleMode(h, mode|flag) == nil {
			restores = append(restores, func() { windows.SetConsoleMode(h, mode) })
		}
	}
	set(out, windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.ENABLE_PROCESSED_OUTPUT|windows.DISABLE_NEWLINE_AUTO_RETURN)
	set(in, windows.ENABLE_VIRTUAL_TERMINAL_INPUT)
	return func() {
		for _, r := range restores {
			r()
		}
	}
}

func notifyTermination(c chan<- os.Signal) { signal.Notify(c, syscall.SIGTERM) }

var procReadConsoleInput = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReadConsoleInputW")

// inputRecord is the Windows INPUT_RECORD with its KEY_EVENT_RECORD view
// (20 bytes).
type inputRecord struct {
	EventType uint16
	_         uint16
	KeyDown   int32
	Repeat    uint16
	VKey      uint16
	VScan     uint16
	Char      uint16
	CtrlState uint32
}

const keyEventType = 0x0001

// consoleReader reads the console input as console input records, not with
// ReadFile: it never blocks on key-up, mouse or focus records (a blocked
// read would swallow the first key typed after the shell ended), and a
// Ctrl+Z (0x1A) is data, not end of file. With virtual-terminal input on,
// every key is a sequence of characters in key-down records.
type consoleReader struct {
	h       windows.Handle
	done    <-chan struct{}
	pending []byte
	high    uint16 // a high surrogate waiting for its low half
}

func newInputReader(f *os.File, done <-chan struct{}) io.Reader {
	return &consoleReader{h: windows.Handle(f.Fd()), done: done}
}

func (r *consoleReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		select {
		case <-r.done:
			return 0, io.EOF
		default:
		}
		ev, err := windows.WaitForSingleObject(r.h, 100)
		if err != nil {
			return 0, err
		}
		if ev != windows.WAIT_OBJECT_0 {
			continue
		}
		select {
		case <-r.done:
			return 0, io.EOF
		default:
		}
		var recs [32]inputRecord
		var n uint32
		// Records are waiting, so this returns at once.
		ret, _, e := procReadConsoleInput.Call(uintptr(r.h), uintptr(unsafe.Pointer(&recs[0])), uintptr(len(recs)), uintptr(unsafe.Pointer(&n)))
		if ret == 0 {
			return 0, e
		}
		for _, rec := range recs[:n] {
			if rec.EventType != keyEventType || rec.KeyDown == 0 || rec.Char == 0 {
				continue
			}
			for i := 0; i < max(int(rec.Repeat), 1); i++ {
				r.add(rec.Char)
			}
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// add appends the UTF-8 form of one UTF-16 unit, joining surrogate pairs.
func (r *consoleReader) add(c uint16) {
	switch {
	case utf16.IsSurrogate(rune(c)) && r.high == 0 && c < 0xDC00:
		r.high = c
		return
	case r.high != 0:
		rr := utf16.DecodeRune(rune(r.high), rune(c))
		r.high = 0
		if rr != utf8.RuneError {
			r.pending = utf8.AppendRune(r.pending, rr)
			return
		}
	}
	r.pending = utf8.AppendRune(r.pending, rune(c))
}

// watchResize polls the console size and delivers it when it changes, until
// done is closed (the Windows console has no SIGWINCH).
func watchResize(done <-chan struct{}, fd int) <-chan [2]int {
	out := make(chan [2]int, 1)
	go func() {
		lw, lh, _ := term.GetSize(fd)
		t := time.NewTicker(300 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				w, h, err := term.GetSize(fd)
				if err != nil || (w == lw && h == lh) {
					continue
				}
				lw, lh = w, h
				select {
				case out <- [2]int{w, h}:
				default:
				}
			}
		}
	}()
	return out
}
