package gui

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"labmill-client/internal/sshx"
	"labmill-client/internal/textsafe"
)

// Terminal events (M62.2). The data is base64 of raw terminal bytes. It is
// the user's own shell: nothing in these events (or typed through
// Terminals.Input) is logged, shown in the chat or sent to Claude.
const (
	EvTermState = "Labmill:term-state" // TermStateEvent: the terminal's connection is up
	EvTermOut   = "Labmill:term-out"   // TermOutEvent: output of the shell
	EvTermExit  = "Labmill:term-exit"  // TermExitEvent: the shell ended
)

// TermStateEvent says the connection of terminal ID is up ("open").
type TermStateEvent struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// TermOutEvent carries a chunk of shell output, base64-encoded.
type TermOutEvent struct {
	ID   string `json:"id"`
	Data string `json:"data"`
}

// TermExitEvent says terminal ID ended: the remote exit status, or Message
// when it couldn't start or the connection dropped.
type TermExitEvent struct {
	ID      string `json:"id"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// TerminalRunner connects to a cluster's login node and runs the shell with
// opts until it ends (launch.Runtime.RunTerminal); it calls connected once
// the connection is up. With allowSignIn false (a chat turn is running) it
// must not open a sign-in dialog: it uses a login the helper already holds
// or fails.
type TerminalRunner func(ctx context.Context, cluster string, opts sshx.ShellOptions, connected func(), allowSignIn bool) (int, error)

// ackWindow is how many bytes of output may be sent to the page before the
// page has confirmed (Ack) it wrote them; beyond that the shell's output
// stalls, the SSH window fills and the remote program pauses.
const ackWindow = 256 * 1024

// ErrPasteTooLarge is returned by Input when the typed or pasted bytes don't
// fit the queue (maxQueuedInput).
var ErrPasteTooLarge = errors.New("paste too large")

// maxReorder is how many input calls may wait for an earlier one.
const maxReorder = 64

// maxTerminals is how many terminals may be open at once.
const maxTerminals = 8

// outChunk is the largest piece of output sent in one event.
const outChunk = 16 * 1024

// Terminals are the terminal tabs of the desktop app: each is a shell on a
// login node, bridged to xterm.js in the page through events.
type Terminals struct {
	emit Emitter
	run  TerminalRunner
	// busy, when set, says a chat turn is running (SetBusy).
	busy func() bool

	mu       sync.Mutex
	next     int
	sessions map[string]*termSession
	closed   bool
}

type termSession struct {
	id      string
	cancel  context.CancelFunc
	in      *inputQueue
	out     *termWriter
	resize  chan [2]int
	userEnd bool // closed by Close: no exit event

	// Wails runs every bound call in its own goroutine, so calls can arrive
	// out of order: Input and Resize carry sequence numbers (seqMu).
	seqMu     sync.Mutex
	nextIn    int64            // the next input sequence number to deliver
	pendingIn map[int64][]byte // later input waiting for nextIn (nil data: rejected)
	resizeSeq int64            // the newest resize sequence number seen
}

// NewTerminals returns the terminal manager; emit delivers events to the page.
func NewTerminals(emit Emitter, run TerminalRunner) *Terminals {
	return &Terminals{emit: emit, run: run, sessions: map[string]*termSession{}}
}

// SetBusy sets the function that says a chat turn is running; while it is,
// a terminal doesn't open a sign-in dialog (it uses an existing login only).
func (t *Terminals) SetBusy(f func() bool) {
	t.mu.Lock()
	t.busy = f
	t.mu.Unlock()
}

// clampSize keeps a terminal size sane (the page sends xterm.js's).
func clampSize(cols, rows int) (int, int) {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	return min(cols, 1000), min(rows, 1000)
}

// Open starts a terminal on the cluster (a name from clusters.yaml), in dir
// ("" for the home folder), sized cols x rows, and returns its ID at once;
// the connection (and a sign-in dialog if the helper holds no login) follows
// in the background and ends in an EvTermState or an EvTermExit. The cluster
// and folder may not contain line breaks or NUL.
func (t *Terminals) Open(cluster, dir string, cols, rows int) (string, error) {
	cluster = strings.TrimSpace(cluster)
	if cluster == "" {
		return "", errors.New("which cluster?")
	}
	if strings.ContainsAny(cluster+dir, "\n\r\x00") {
		return "", errors.New("the cluster and folder can't contain line breaks or NUL")
	}
	if len(dir) > 4096 {
		return "", errors.New("that folder name is too long")
	}
	if t.run == nil {
		return "", errors.New("the terminal isn't available yet")
	}
	cols, rows = clampSize(cols, rows)
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return "", errors.New("Labmill is closing")
	}
	if len(t.sessions) >= maxTerminals {
		t.mu.Unlock()
		return "", fmt.Errorf("%d terminals are open; close one first", maxTerminals)
	}
	t.next++
	id := "t" + strconv.Itoa(t.next)
	ctx, cancel := context.WithCancel(context.Background())
	s := &termSession{id: id, cancel: cancel, in: newInputQueue(), out: newTermWriter(id, t.emit),
		resize: make(chan [2]int, 8), pendingIn: map[int64][]byte{}}
	t.sessions[id] = s
	allowSignIn := t.busy == nil || !t.busy()
	t.mu.Unlock()
	go t.runSession(ctx, s, cluster, dir, cols, rows, allowSignIn)
	return id, nil
}

func (t *Terminals) runSession(ctx context.Context, s *termSession, cluster, dir string, cols, rows int, allowSignIn bool) {
	defer s.cancel()
	out := s.out
	opts := sshx.ShellOptions{
		Dir: dir, Width: cols, Height: rows, Resize: s.resize,
		Stdin: s.in, Stdout: out, Stderr: out,
	}
	code, err := t.run(ctx, cluster, opts, func() {
		t.emit(EvTermState, TermStateEvent{ID: s.id, State: "open"})
	}, allowSignIn)
	s.in.close()
	s.out.close()
	t.mu.Lock()
	delete(t.sessions, s.id)
	userEnd := s.userEnd
	t.mu.Unlock()
	if userEnd {
		return
	}
	ev := TermExitEvent{ID: s.id, Code: code}
	switch {
	case err == nil:
	case errors.Is(err, sshx.ErrShellClosed):
		ev.Message = "The connection closed."
	case errors.Is(err, context.Canceled):
	default:
		ev.Message = textsafe.Clean(err.Error())
	}
	t.emit(EvTermExit, ev)
}

func (t *Terminals) get(id string) (*termSession, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.sessions[id]
	if !ok {
		return nil, errors.New("that terminal has ended")
	}
	return s, nil
}

// Input sends what the user typed (base64 of the bytes) to the shell. seq
// numbers the calls of one terminal from 0 up, one per call: calls that
// arrive out of order wait for the earlier ones, so the bytes reach the
// shell in the order typed. It never blocks: input typed before the
// connection is up waits for it. A call that fails (bad data, ErrPasteTooLarge)
// still uses its number.
func (t *Terminals) Input(id string, seq int64, data string) error {
	s, err := t.get(id)
	if err != nil {
		return err
	}
	b, derr := base64.StdEncoding.DecodeString(data)
	if derr != nil {
		derr = errors.New("bad terminal input")
	} else if len(b) > maxQueuedInput {
		derr = ErrPasteTooLarge
	}
	if derr != nil {
		b = nil
	}
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	if seq < s.nextIn {
		return derr // a repeat
	}
	if seq != s.nextIn {
		if len(s.pendingIn) >= maxReorder {
			return errors.New("terminal input arrived out of order")
		}
		s.pendingIn[seq] = b
		return derr
	}
	var werr error
	deliver := func(b []byte) {
		if len(b) > 0 {
			if err := s.in.write(b); err != nil {
				werr = err
			}
		}
	}
	deliver(b)
	s.nextIn++
	for {
		nb, ok := s.pendingIn[s.nextIn]
		if !ok {
			break
		}
		delete(s.pendingIn, s.nextIn)
		deliver(nb)
		s.nextIn++
	}
	if derr != nil {
		return derr
	}
	return werr
}

// Resize tells the shell the terminal is now cols x rows cells. seq numbers
// the calls from 1 up; a call older than one already seen is ignored (the
// newest size wins).
func (t *Terminals) Resize(id string, seq int64, cols, rows int) error {
	s, err := t.get(id)
	if err != nil {
		return err
	}
	if cols <= 0 || rows <= 0 {
		return nil
	}
	cols, rows = clampSize(cols, rows)
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	if seq <= s.resizeSeq {
		return nil
	}
	s.resizeSeq = seq
	// Only the newest size matters: drop an older one if the queue is full.
	for {
		select {
		case s.resize <- [2]int{cols, rows}:
			return nil
		default:
			select {
			case <-s.resize:
			default:
			}
		}
	}
}

// Ack tells the terminal the page wrote n bytes of output (flow control: the
// shell's output stalls while more than ackWindow bytes are unconfirmed).
func (t *Terminals) Ack(id string, n int) error {
	t.mu.Lock()
	s, ok := t.sessions[id]
	t.mu.Unlock()
	if !ok {
		return nil
	}
	s.out.ack(n)
	return nil
}

// Close ends a terminal (closing its tab): the session and its connection
// are closed and no exit event follows.
func (t *Terminals) Close(id string) error {
	t.mu.Lock()
	s, ok := t.sessions[id]
	if ok {
		s.userEnd = true
	}
	t.mu.Unlock()
	if !ok {
		return nil
	}
	s.cancel()
	s.in.close()
	s.out.close()
	return nil
}

// Count returns how many terminals are open.
func (t *Terminals) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.sessions)
}

// CloseAll ends every terminal and refuses new ones (shutdown).
func (t *Terminals) CloseAll() {
	t.mu.Lock()
	t.closed = true
	var ids []string
	for id := range t.sessions {
		ids = append(ids, id)
	}
	t.mu.Unlock()
	for _, id := range ids {
		t.Close(id)
	}
}

// termWriter turns shell output into EvTermOut events, in order. It blocks
// while ackWindow bytes are unconfirmed, until ack or close.
type termWriter struct {
	id      string
	emit    Emitter
	mu      sync.Mutex
	cond    *sync.Cond
	unacked int
	closed  bool
}

func newTermWriter(id string, emit Emitter) *termWriter {
	w := &termWriter{id: id, emit: emit}
	w.cond = sync.NewCond(&w.mu)
	return w
}

func (w *termWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	done := 0
	for rest := p; len(rest) > 0; {
		for w.unacked >= ackWindow && !w.closed {
			w.cond.Wait()
		}
		if w.closed {
			return done, io.ErrClosedPipe
		}
		n := min(len(rest), outChunk)
		w.unacked += n
		w.emit(EvTermOut, TermOutEvent{ID: w.id, Data: base64.StdEncoding.EncodeToString(rest[:n])})
		rest = rest[n:]
		done += n
	}
	return len(p), nil
}

// ack records that the page wrote n bytes.
func (w *termWriter) ack(n int) {
	w.mu.Lock()
	w.unacked = max(0, w.unacked-max(n, 0))
	w.cond.Broadcast()
	w.mu.Unlock()
}

// close wakes a blocked Write; later writes fail.
func (w *termWriter) close() {
	w.mu.Lock()
	w.closed = true
	w.cond.Broadcast()
	w.mu.Unlock()
}

// inputQueue is the shell's stdin: writes never block (typing before the
// connection is up is kept, up to maxQueuedInput bytes), reads wait for data.
type inputQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
}

const maxQueuedInput = 1 << 20

func newInputQueue() *inputQueue {
	q := &inputQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *inputQueue) write(b []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil
	}
	if len(q.buf)+len(b) > maxQueuedInput {
		return ErrPasteTooLarge
	}
	q.buf = append(q.buf, b...)
	q.cond.Broadcast()
	return nil
}

func (q *inputQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

func (q *inputQueue) Read(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.buf) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, q.buf)
	q.buf = q.buf[n:]
	return n, nil
}
