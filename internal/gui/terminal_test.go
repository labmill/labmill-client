package gui

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"labmill-client/internal/sshx"
	"labmill-client/internal/store"
)

type termEvents struct {
	mu sync.Mutex
	ev []struct {
		name string
		data any
	}
}

func (r *termEvents) emit(name string, data any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev = append(r.ev, struct {
		name string
		data any
	}{name, data})
}

// output joins the decoded EvTermOut data of terminal id.
func (r *termEvents) output(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var sb strings.Builder
	for _, e := range r.ev {
		if o, ok := e.data.(TermOutEvent); ok && e.name == EvTermOut && o.ID == id {
			b, _ := base64.StdEncoding.DecodeString(o.Data)
			sb.Write(b)
		}
	}
	return sb.String()
}

func (r *termEvents) exits() []TermExitEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []TermExitEvent
	for _, e := range r.ev {
		if x, ok := e.data.(TermExitEvent); ok && e.name == EvTermExit {
			out = append(out, x)
		}
	}
	return out
}

func waitTerm(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// echoRunner is a shell that greets, echoes lines until "exit" (status 3) and
// records the options it got.
func echoRunner(got chan<- sshx.ShellOptions) TerminalRunner {
	return func(ctx context.Context, cluster string, opts sshx.ShellOptions, connected func(), allowSignIn bool) (int, error) {
		if got != nil {
			got <- opts
		}
		connected()
		io.WriteString(opts.Stdout, "welcome "+cluster+"\r\n")
		buf := make([]byte, 64)
		for {
			n, err := opts.Stdin.Read(buf)
			if n > 0 {
				s := string(buf[:n])
				if strings.Contains(s, "exit") {
					return 3, nil
				}
				io.WriteString(opts.Stdout, "got:"+s)
			}
			if err != nil || ctx.Err() != nil {
				return 0, ctx.Err()
			}
		}
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestTerminalOpenInputExit(t *testing.T) {
	ev := &termEvents{}
	got := make(chan sshx.ShellOptions, 1)
	ts := NewTerminals(ev.emit, echoRunner(got))
	id, err := ts.Open("bigcluster", "/data/my group", 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	opts := <-got
	if opts.Dir != "/data/my group" || opts.Width != 100 || opts.Height != 30 {
		t.Fatalf("opts = %+v", opts)
	}
	waitTerm(t, "welcome", func() bool { return strings.Contains(ev.output(id), "welcome bigcluster") })
	// Input typed before and after the connection is up, with non-ASCII bytes.
	if err := ts.Input(id, 0, b64("ls é\n")); err != nil {
		t.Fatal(err)
	}
	waitTerm(t, "echo", func() bool { return strings.Contains(ev.output(id), "got:ls é") })
	if err := ts.Resize(id, 1, 120, 40); err != nil {
		t.Fatal(err)
	}
	select {
	case sz := <-opts.Resize:
		if sz != [2]int{120, 40} {
			t.Fatalf("resize = %v", sz)
		}
	case <-time.After(time.Second):
		t.Fatal("no resize")
	}
	ts.Input(id, 1, b64("exit\n"))
	waitTerm(t, "exit event", func() bool { return len(ev.exits()) == 1 })
	if x := ev.exits()[0]; x.ID != id || x.Code != 3 || x.Message != "" {
		t.Fatalf("exit = %+v", x)
	}
	waitTerm(t, "session removed", func() bool { return ts.Count() == 0 })
	if err := ts.Input(id, 9, b64("x")); err == nil {
		t.Error("input to an ended terminal was accepted")
	}
}

func TestTerminalCloseSendsNoExit(t *testing.T) {
	ev := &termEvents{}
	ts := NewTerminals(ev.emit, echoRunner(nil))
	a, _ := ts.Open("c1", "", 0, 0)
	b, _ := ts.Open("c2", "", 0, 0)
	if a == b {
		t.Fatal("same ID twice")
	}
	waitTerm(t, "both open", func() bool {
		return strings.Contains(ev.output(a), "welcome c1") && strings.Contains(ev.output(b), "welcome c2")
	})
	if ts.Count() != 2 {
		t.Fatalf("count = %d", ts.Count())
	}
	ts.Close(a)
	waitTerm(t, "one left", func() bool { return ts.Count() == 1 })
	time.Sleep(30 * time.Millisecond)
	if n := len(ev.exits()); n != 0 {
		t.Fatalf("a closed tab sent %d exit events", n)
	}
	ts.CloseAll()
	waitTerm(t, "none left", func() bool { return ts.Count() == 0 })
	if _, err := ts.Open("c3", "", 0, 0); err == nil {
		t.Error("a terminal opened after CloseAll")
	}
}

func TestTerminalErrorsAndValidation(t *testing.T) {
	ev := &termEvents{}
	run := func(ctx context.Context, cluster string, opts sshx.ShellOptions, connected func(), allowSignIn bool) (int, error) {
		switch cluster {
		case "dropped":
			return 0, sshx.ErrShellClosed
		}
		return 0, errors.New("the sign-in was canceled\x1b[31m")
	}
	ts := NewTerminals(ev.emit, run)
	for _, bad := range [][2]string{{"", ""}, {"a\nb", ""}, {"a", "x\ry"}, {"a", "x\x00y"}, {"a", strings.Repeat("d", 5000)}} {
		if _, err := ts.Open(bad[0], bad[1], 80, 24); err == nil {
			t.Errorf("Open(%q, %q) was accepted", bad[0], bad[1])
		}
	}
	ts.Open("dropped", "", 80, 24)
	ts.Open("other", "", 80, 24)
	waitTerm(t, "two exits", func() bool { return len(ev.exits()) == 2 })
	var msgs []string
	for _, x := range ev.exits() {
		msgs = append(msgs, x.Message)
		if strings.ContainsRune(x.Message, 0x1b) {
			t.Errorf("escape in the message %q", x.Message)
		}
	}
	joined := strings.Join(msgs, "|")
	if !strings.Contains(joined, "connection closed") || !strings.Contains(joined, "sign-in was canceled") {
		t.Errorf("messages = %q", msgs)
	}
	ts2 := NewTerminals(ev.emit, echoRunner(nil))
	defer ts2.CloseAll()
	var err error
	for j := 0; j <= maxTerminals && err == nil; j++ {
		_, err = ts2.Open("c", "", 80, 24)
	}
	if err == nil {
		t.Fatal("no terminal limit")
	}
}

func TestTerminalOutputChunks(t *testing.T) {
	ev := &termEvents{}
	big := strings.Repeat("x", 3*outChunk+5)
	run := func(ctx context.Context, cluster string, opts sshx.ShellOptions, connected func(), allowSignIn bool) (int, error) {
		io.WriteString(opts.Stdout, big)
		return 0, nil
	}
	ts := NewTerminals(ev.emit, run)
	id, _ := ts.Open("c", "", 80, 24)
	waitTerm(t, "exit", func() bool { return len(ev.exits()) == 1 })
	if got := ev.output(id); got != big {
		t.Fatalf("got %d bytes, want %d", len(got), len(big))
	}
	ev.mu.Lock()
	defer ev.mu.Unlock()
	for _, e := range ev.ev {
		if o, ok := e.data.(TermOutEvent); ok {
			if b, _ := base64.StdEncoding.DecodeString(o.Data); len(b) > outChunk {
				t.Fatalf("chunk of %d bytes", len(b))
			}
		}
	}
}

func TestJobTerminalTarget(t *testing.T) {
	a := NewApp(NewBridge(func(string, any) {}), nil, nil, "")
	if _, _, err := a.JobTerminalTarget(1); err == nil {
		t.Error("no job lister: want an error")
	}
	jobs := []store.Job{
		{ID: 1, Cluster: "c1", RemoteID: "9", RemoteDir: "/scratch/one"},
		{ID: 2, Cluster: "c1", RemoteID: "9"},                     // no folder
		{ID: 3, Cluster: "c1", RemoteDir: "/scratch/draft"},       // never submitted
		{ID: 4, Cluster: "c1", RemoteID: "9", RemoteDir: "/a\nb"}, // hostile folder
	}
	a = NewApp(NewBridge(func(string, any) {}), nil, func(context.Context) ([]store.Job, error) { return jobs, nil }, "")
	c, d, err := a.JobTerminalTarget(1)
	if err != nil || c != "c1" || d != "/scratch/one" {
		t.Fatalf("job 1 = %q %q %v", c, d, err)
	}
	for _, id := range []int64{2, 3, 4, 99} {
		if _, _, err := a.JobTerminalTarget(id); err == nil {
			t.Errorf("job %d was accepted", id)
		}
	}
	// The jobs panel offers the button under the same condition.
	views, _ := a.Jobs()
	for _, v := range views {
		_, _, err := a.JobTerminalTarget(v.ID)
		if v.Terminal != (err == nil) {
			t.Errorf("job %d: Terminal=%v but target err=%v", v.ID, v.Terminal, err)
		}
	}
}

// Calls arrive in any order (Wails runs each in its own goroutine); the
// shell gets the bytes in sequence order.
func TestTerminalInputOrdered(t *testing.T) {
	ev := &termEvents{}
	var mu sync.Mutex
	var got strings.Builder
	release := make(chan struct{})
	run := func(ctx context.Context, cluster string, opts sshx.ShellOptions, connected func(), allowSignIn bool) (int, error) {
		<-release
		buf := make([]byte, 64)
		for {
			n, err := opts.Stdin.Read(buf)
			mu.Lock()
			got.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return 0, nil
			}
		}
	}
	ts := NewTerminals(ev.emit, run)
	id, _ := ts.Open("c", "", 80, 24)
	const n = maxReorder - 4
	var wg sync.WaitGroup
	for i := n - 1; i >= 0; i-- { // the worst order: reversed, all at once
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ts.Input(id, int64(i), b64(fmt.Sprintf("%03d,", i)))
		}(i)
	}
	wg.Wait()
	close(release)
	var want strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&want, "%03d,", i)
	}
	waitTerm(t, "all input", func() bool { mu.Lock(); defer mu.Unlock(); return got.Len() >= want.Len() })
	mu.Lock()
	defer mu.Unlock()
	if got.String()[:want.Len()] != want.String() {
		t.Fatalf("input out of order: %q", got.String()[:80])
	}
}

func TestTerminalResizeNewestWins(t *testing.T) {
	ev := &termEvents{}
	ch := make(chan sshx.ShellOptions, 1)
	ts := NewTerminals(ev.emit, echoRunner(ch))
	id, _ := ts.Open("c", "", 80, 24)
	opts := <-ch
	ts.Resize(id, 3, 130, 50)
	ts.Resize(id, 2, 90, 30) // older: ignored
	if sz := <-opts.Resize; sz != [2]int{130, 50} {
		t.Fatalf("size = %v", sz)
	}
	select {
	case sz := <-opts.Resize:
		t.Fatalf("stale resize delivered: %v", sz)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestTerminalPasteTooLarge(t *testing.T) {
	ev := &termEvents{}
	block := make(chan struct{})
	run := func(ctx context.Context, cluster string, opts sshx.ShellOptions, connected func(), allowSignIn bool) (int, error) {
		<-block // never reads: the queue fills
		return 0, nil
	}
	ts := NewTerminals(ev.emit, run)
	defer close(block)
	id, _ := ts.Open("c", "", 80, 24)
	big := b64(strings.Repeat("x", maxQueuedInput+1))
	if err := ts.Input(id, 0, big); !errors.Is(err, ErrPasteTooLarge) {
		t.Fatalf("one large paste: %v", err)
	}
	half := b64(strings.Repeat("x", maxQueuedInput/2+1))
	if err := ts.Input(id, 1, half); err != nil {
		t.Fatal(err)
	}
	if err := ts.Input(id, 2, half); !errors.Is(err, ErrPasteTooLarge) {
		t.Fatalf("queue overflow: %v", err)
	}
	// A rejected call still used its number: the next one is delivered, not stuck.
	if err := ts.Input(id, 3, b64("a")); err != nil {
		t.Fatal(err)
	}
}

// The writer stalls after ackWindow unacknowledged bytes, resumes on ack and
// is woken by close.
func TestTermWriterFlowControl(t *testing.T) {
	var mu sync.Mutex
	emitted := 0
	w := newTermWriter("t1", func(name string, data any) {
		b, _ := base64.StdEncoding.DecodeString(data.(TermOutEvent).Data)
		mu.Lock()
		emitted += len(b)
		mu.Unlock()
	})
	count := func() int { mu.Lock(); defer mu.Unlock(); return emitted }
	total := ackWindow + 4*outChunk
	done := make(chan error, 1)
	go func() { _, err := w.Write(make([]byte, total)); done <- err }()
	waitTerm(t, "the window", func() bool { return count() >= ackWindow })
	select {
	case <-done:
		t.Fatal("Write finished without acks")
	case <-time.After(50 * time.Millisecond):
	}
	if c := count(); c != ackWindow {
		t.Fatalf("emitted %d before any ack, want %d", c, ackWindow)
	}
	w.ack(ackWindow)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if count() != total {
		t.Fatalf("emitted %d, want %d", count(), total)
	}
	// Closing wakes a blocked writer.
	w2 := newTermWriter("t2", func(string, any) {})
	go func() { _, err := w2.Write(make([]byte, 2*ackWindow)); done <- err }()
	time.Sleep(30 * time.Millisecond)
	w2.close()
	select {
	case err := <-done:
		if err == nil {
			t.Error("write on a closed terminal succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("close didn't wake the writer")
	}
}

// Closing a terminal frees a shell stuck on unacknowledged output.
func TestTerminalCloseUnblocksOutput(t *testing.T) {
	ev := &termEvents{}
	finished := make(chan struct{})
	run := func(ctx context.Context, cluster string, opts sshx.ShellOptions, connected func(), allowSignIn bool) (int, error) {
		opts.Stdout.Write(make([]byte, 3*ackWindow))
		close(finished)
		return 0, nil
	}
	ts := NewTerminals(ev.emit, run)
	id, _ := ts.Open("c", "", 80, 24)
	time.Sleep(30 * time.Millisecond)
	ts.Close(id)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("the shell stayed blocked after Close")
	}
}

// The runner is told whether signing in is allowed: not while a turn runs.
func TestTerminalSignInOnlyWhenIdle(t *testing.T) {
	ev := &termEvents{}
	allowed := make(chan bool, 2)
	run := func(ctx context.Context, cluster string, opts sshx.ShellOptions, connected func(), allowSignIn bool) (int, error) {
		allowed <- allowSignIn
		return 0, nil
	}
	ts := NewTerminals(ev.emit, run)
	busy := true
	ts.SetBusy(func() bool { return busy })
	ts.Open("c", "", 80, 24)
	busy = false
	ts.Open("c", "", 80, 24)
	a, b := <-allowed, <-allowed
	if a == b {
		t.Errorf("allowSignIn = %v and %v, want one of each", a, b)
	}
}
