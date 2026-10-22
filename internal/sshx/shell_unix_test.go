//go:build !windows

package sshx

import (
	"io"
	"os"
	"testing"
	"time"
)

// After done is closed, the input reader returns EOF and leaves the bytes in
// the terminal for whoever reads next.
func TestPollReaderLeavesInputAfterDone(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	done := make(chan struct{})
	rd := newInputReader(r, done)

	got := make(chan string, 1)
	go func() {
		b := make([]byte, 16)
		n, _ := rd.Read(b)
		got <- string(b[:n])
	}()
	w.WriteString("ab")
	if s := <-got; s != "ab" {
		t.Fatalf("read %q", s)
	}

	// A read waiting when done closes ends without taking later input.
	ended := make(chan error, 1)
	go func() {
		_, err := rd.Read(make([]byte, 16))
		ended <- err
	}()
	time.Sleep(50 * time.Millisecond)
	close(done)
	select {
	case err := <-ended:
		if err != io.EOF {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not stop")
	}
	w.WriteString("next")
	if _, err := rd.Read(make([]byte, 16)); err != io.EOF {
		t.Fatalf("read after done: %v", err)
	}
	b := make([]byte, 16)
	n, _ := r.Read(b)
	if string(b[:n]) != "next" {
		t.Errorf("input left = %q", b[:n])
	}
}
