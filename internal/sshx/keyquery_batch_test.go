package sshx

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx/sshtest"
)

// limitProxy listens on 127.0.0.1 and forwards the connections for which allow
// (given the 1-based count of connections so far) is true to target; the others
// are accepted and closed at once, like OpenSSH's per-source penalties do.
// conns counts every accepted connection.
func limitProxy(t *testing.T, target string, allow func(n int) bool) (addr string, conns *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var n atomic.Int32
	var wg sync.WaitGroup
	t.Cleanup(func() { ln.Close(); wg.Wait() })
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			i := int(n.Add(1))
			if !allow(i) {
				c.Close()
				continue
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				continue
			}
			wg.Add(2)
			go func() { defer wg.Done(); io.Copy(up, c); up.Close() }()
			go func() { defer wg.Done(); io.Copy(c, up); c.Close() }()
		}
	}()
	return ln.Addr().String(), &n
}

// proxiedAlias points the alias testAlias at a limitProxy in front of srv and
// trusts srv's host key under the proxy's address.
func proxiedAlias(t *testing.T, p sshconfig.Paths, srv *sshtest.Server, allow func(n int) bool) *atomic.Int32 {
	t.Helper()
	addr, conns := limitProxy(t, srv.Addr, allow)
	h, port, _ := net.SplitHostPort(addr)
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: testAlias, HostName: h, User: "alice", Port: atoi(t, port)}}, "linux"); err != nil {
		t.Fatal(err)
	}
	line := knownhosts.Line([]string{knownhosts.Normalize(addr)}, srv.HostKey) + "\n"
	if err := os.WriteFile(filepath.Join(p.SSHDir, "known_hosts"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return conns
}

// testAlias is the ssh config alias of the proxied host in these tests.
const testAlias = "bigcluster"

func always(int) bool { return true }

// firstN lets the first n connections through and refuses the rest.
func firstN(n int) func(int) bool { return func(i int) bool { return i <= n } }

// The default pauses are for real servers; tests run with short ones.
func init() { queryPause, limitWait = 0, time.Millisecond }

// rejectedOnce is a connect func for KeyGuide.Run: the first call fails like
// a key-only host without a usable key, the next ones stop the guide.
func rejectedOnce() func(context.Context) (*ssh.Client, error) {
	n := 0
	return func(context.Context) (*ssh.Client, error) {
		if n++; n == 1 {
			return nil, ErrKeyRejected
		}
		return nil, errors.New("stop here")
	}
}

func setQueryPause(t *testing.T, d time.Duration) {
	t.Helper()
	old := queryPause
	queryPause = d
	t.Cleanup(func() { queryPause = old })
}

func TestQueryKeysBatches(t *testing.T) {
	tests := []struct {
		name  string
		n     int
		ok    []int
		conns int32
	}{
		{"accepted third of four", 4, []int{2}, 2},
		{"four rejected", 4, nil, 1},
		{"twelve rejected", 12, nil, 3},
		{"accepted first and seventh of eight", 8, []int{0, 6}, 4},
		{"accepted last of five", 5, []int{4}, 1},
		{"five rejected", 5, nil, 1},
		{"six rejected", 6, nil, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setQueryPause(t, 0)
			pubs := make([]ssh.PublicKey, tt.n)
			want := make([]bool, tt.n)
			var auth []ssh.PublicKey
			for i := range pubs {
				pubs[i] = edPub(t)
			}
			for _, i := range tt.ok {
				want[i] = true
				auth = append(auth, pubs[i])
			}
			// MaxAuthTries 6 is sshd's default: the batch size keeps under it.
			p, srv := keyOnlyAlias(t, sshtest.Options{AuthorizedKeys: auth, MaxAuthTries: 6})
			conns := proxiedAlias(t, p, srv, always)
			got, err := QueryKeys(context.Background(), p, testAlias, &scriptPrompter{}, LoginOptions{}, pubs)
			if err != nil {
				t.Fatalf("QueryKeys: %v", err)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("key %d accepted = %v, want %v", i, got[i], want[i])
				}
			}
			if conns.Load() != tt.conns {
				t.Errorf("connections = %d, want %d", conns.Load(), tt.conns)
			}
			if srv.Logins() != 0 {
				t.Errorf("logins = %d", srv.Logins())
			}
		})
	}
}

// A server whose MaxAuthTries is below the batch size hangs up mid-batch;
// QueryKeys offers fewer keys at once from then on and still finds the key.
func TestQueryKeysLowMaxAuthTries(t *testing.T) {
	for _, tries := range []int{1, 2, 3, 4, 5} {
		t.Run("MaxAuthTries "+strconv.Itoa(tries), func(t *testing.T) {
			pubs := make([]ssh.PublicKey, 8)
			for i := range pubs {
				pubs[i] = edPub(t)
			}
			p, srv := keyOnlyAlias(t, sshtest.Options{AuthorizedKeys: []ssh.PublicKey{pubs[6]}, MaxAuthTries: tries})
			conns := proxiedAlias(t, p, srv, always)
			got, err := QueryKeys(context.Background(), p, testAlias, &scriptPrompter{}, LoginOptions{}, pubs)
			if err != nil {
				t.Fatalf("QueryKeys: %v", err)
			}
			for i, g := range got {
				if g != (i == 6) {
					t.Errorf("key %d accepted = %v", i, g)
				}
			}
			if conns.Load() > 20 {
				t.Errorf("connections = %d, no progress", conns.Load())
			}
		})
	}
}

func TestTooManyTries(t *testing.T) {
	for _, s := range []string{`ssh: handshake failed: ssh: disconnect, reason 2: "too many authentication failures"`, "Too many authentication failures"} {
		if !tooManyTries(errors.New(s)) {
			t.Errorf("%q not recognized", s)
		}
	}
	if tooManyTries(errors.New("ssh: handshake failed: EOF")) {
		t.Error("EOF taken for too many tries")
	}
}

func TestQueryKeysHostLimiting(t *testing.T) {
	setQueryPause(t, 0)
	pubs := make([]ssh.PublicKey, 13)
	for i := range pubs {
		pubs[i] = edPub(t)
	}
	// The accepted key 2 is found on the first connection; the second one is
	// refused before the handshake.
	p, srv := keyOnlyAlias(t, sshtest.Options{AuthorizedKeys: []ssh.PublicKey{pubs[2]}})
	conns := proxiedAlias(t, p, srv, firstN(1))
	got, err := QueryKeys(context.Background(), p, testAlias, &scriptPrompter{}, LoginOptions{}, pubs)
	if !errors.Is(err, ErrHostLimiting) {
		t.Fatalf("err = %v, want ErrHostLimiting", err)
	}
	if strings.Contains(err.Error(), "handshake") {
		t.Errorf("error shows the raw handshake failure: %v", err)
	}
	if len(got) != len(pubs) {
		t.Fatalf("partial result has %d entries, want %d", len(got), len(pubs))
	}
	for i, g := range got {
		if g != (i == 2) {
			t.Errorf("key %d = %v", i, g)
		}
	}
	if conns.Load() != 2 {
		t.Errorf("connections = %d, want 2 (no retry after the refusal)", conns.Load())
	}
}

func TestQueryKeysPause(t *testing.T) {
	const pause = 40 * time.Millisecond
	setQueryPause(t, pause)
	pubs := make([]ssh.PublicKey, 11) // three connections, two pauses
	for i := range pubs {
		pubs[i] = edPub(t)
	}
	p, srv := keyOnlyAlias(t, sshtest.Options{})
	trustHost(t, p, srv)
	start := time.Now()
	if _, err := QueryKeys(context.Background(), p, "orca", &scriptPrompter{}, LoginOptions{}, pubs); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 2*pause {
		t.Errorf("elapsed %v, want at least %v", el, 2*pause)
	}
}

func TestQueryKeysCancelDuringPause(t *testing.T) {
	setQueryPause(t, time.Minute)
	pubs := make([]ssh.PublicKey, 6)
	for i := range pubs {
		pubs[i] = edPub(t)
	}
	p, srv := keyOnlyAlias(t, sshtest.Options{})
	trustHost(t, p, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, err := QueryKeys(ctx, p, "orca", &scriptPrompter{}, LoginOptions{}, pubs)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context error", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("the pause did not honor the context")
	}
	if len(got) != len(pubs) {
		t.Errorf("partial result has %d entries", len(got))
	}
}

func TestSleepCtx(t *testing.T) {
	if err := sleepCtx(context.Background(), 0); err != nil {
		t.Error(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestClosedEarly(t *testing.T) {
	for _, e := range []error{io.EOF, errors.New("ssh: handshake failed: EOF"), errors.New("read tcp: connection reset by peer"), errors.New("wsarecv: An existing connection was forcibly closed by the remote host."), syscall.Errno(10054)} {
		if !closedEarly(e) {
			t.Errorf("%v not seen as closed early", e)
		}
	}
	if closedEarly(errors.New("ssh: handshake failed: no common algorithm")) {
		t.Error("other error seen as closed early")
	}
}

// --- key reuse in the guide ---

// workKeys writes n local key pairs work_a, work_b, ... into the ssh folder
// (names the normal connect doesn't offer) and returns their public keys.
func workKeys(t *testing.T, p sshconfig.Paths, n int) []ssh.PublicKey {
	t.Helper()
	var pubs []ssh.PublicKey
	for i := 0; i < n; i++ {
		priv, pub := newKeyPair(t, "")
		name := "work_" + string(rune('a'+i))
		writeFile(t, filepath.Join(p.SSHDir, name), string(priv))
		writeFile(t, filepath.Join(p.SSHDir, name+".pub"), keyLine(pub, "me"))
		pubs = append(pubs, pub)
	}
	return pubs
}

// proxiedGuide is reuseGuide for the proxied alias.
func proxiedGuide(p sshconfig.Paths, srv *sshtest.Server, pr Prompter) *KeyGuide {
	g := reuseGuide(p, srv, pr, nil)
	g.Alias = testAlias
	return g
}

// Refused connections never turn into an offer to register a new key: the
// guide waits, asks again, and then ends with a plain message.
func TestKeyGuideHostLimitingEndsGuide(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	p, srv := keyOnlyEnv(t)
	conns := proxiedAlias(t, p, srv, firstN(0)) // refuses every connection
	workKeys(t, p, 1)
	pr := guideAnswers{create: "yes"}.prompter()
	g := proxiedGuide(p, srv, pr)
	_, err := g.Run(context.Background(), rejectedOnce())
	if !errors.Is(err, ErrHostLimiting) {
		t.Fatalf("Run = %v, want the limiting error", err)
	}
	if !strings.Contains(err.Error(), "still refusing connections") || strings.Contains(err.Error(), "handshake") {
		t.Errorf("error text = %q", err)
	}
	if pr.count(AskKeyCreate) != 0 {
		t.Error("offered to create a new key while the host was only refusing connections")
	}
	if conns.Load() != 2 {
		t.Errorf("connections = %d, want 2 (one try, one retry)", conns.Load())
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	var found bool
	for _, n := range pr.notified {
		if strings.Contains(n, "briefly refusing") && strings.Contains(n, "checks again") {
			found = true
		}
		if strings.Contains(n, "handshake") || strings.Contains(n, "EOF") {
			t.Errorf("raw error shown: %q", n)
		}
	}
	if !found {
		t.Errorf("no limiting notice in %q", pr.notified)
	}
}

// The retry asks only the keys not asked before and finds the accepted one.
func TestKeyGuideHostLimitingRetryFindsKey(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	p, srv := keyOnlyEnv(t)
	keys := workKeys(t, p, 7) // 5 rejected on connection 1; 2 left for the retry
	srv.Authorize(keys[6])
	conns := proxiedAlias(t, p, srv, func(n int) bool { return n != 2 }) // connection 2 is refused
	pr := &scriptPrompter{onAsk: func(q Question) (string, error) { return "", errors.New("unexpected " + q.Prompt) }}
	g := proxiedGuide(p, srv, pr)
	if _, err := g.Run(context.Background(), rejectedOnce()); err == nil || err.Error() != "stop here" {
		t.Fatalf("Run = %v, want to reconnect with the accepted key (connections %d)", err, conns.Load())
	}
	h, _, _ := sshconfig.Lookup(p, testAlias)
	if len(h.IdentityFiles) != 1 || h.IdentityFiles[0] != "~/.ssh/work_g" {
		t.Errorf("IdentityFile = %v", h.IdentityFiles)
	}
	if conns.Load() != 3 {
		t.Errorf("connections = %d, want 3", conns.Load())
	}
}

func TestKeyGuideAcceptedKeyBeforeLimiting(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	p, srv := keyOnlyEnv(t)
	// The first key is accepted on connection 1; the second needs connection
	// 2, which the host refuses (and keeps refusing).
	keys := workKeys(t, p, 2)
	srv.Authorize(keys[0])
	conns := proxiedAlias(t, p, srv, firstN(1))
	pr := &scriptPrompter{onAsk: func(q Question) (string, error) { return "", errors.New("unexpected " + q.Prompt) }}
	g := proxiedGuide(p, srv, pr)
	if _, err := g.Run(context.Background(), rejectedOnce()); err == nil || err.Error() != "stop here" {
		t.Fatalf("Run = %v, want to reconnect with the accepted key (connections %d)", err, conns.Load())
	}
	h, _, _ := sshconfig.Lookup(p, testAlias)
	if len(h.IdentityFiles) != 1 || h.IdentityFiles[0] != "~/.ssh/work_a" {
		t.Errorf("IdentityFile = %v", h.IdentityFiles)
	}
}
