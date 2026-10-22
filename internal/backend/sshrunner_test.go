package backend

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/sshx/sshtest"
)

func dialTest(t *testing.T, cmds map[string]sshtest.Handler) *SSHRunner {
	t.Helper()
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Commands: cmds})
	cfg := &ssh.ClientConfig{
		User: "alice",
		Auth: []ssh.AuthMethod{ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			ans := make([]string, len(qs))
			for i := range ans {
				ans[i] = "pw"
			}
			return ans, nil
		})},
		HostKeyCallback: ssh.FixedHostKey(srv.HostKey),
		Timeout:         5 * time.Second,
	}
	c, err := ssh.Dial("tcp", srv.Addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &SSHRunner{Client: c}
}

func TestSSHRunner(t *testing.T) {
	r := dialTest(t, map[string]sshtest.Handler{
		"echoargs": func(args string, _ io.Reader, out, _ io.Writer) int { fmt.Fprint(out, args); return 0 },
		"fail":     func(_ string, _ io.Reader, _, errw io.Writer) int { fmt.Fprint(errw, "boom"); return 3 },
		"cat": func(_ string, in io.Reader, out, _ io.Writer) int {
			io.Copy(out, in)
			return 0
		},
		"big": func(_ string, _ io.Reader, out, _ io.Writer) int {
			out.Write([]byte(strings.Repeat("x", maxCapture+1000)))
			return 0
		},
	})
	ctx := context.Background()

	res, err := r.Run(ctx, []string{"fail"}, nil)
	if err != nil || res.ExitCode != 3 || res.Stderr != "boom" {
		t.Errorf("fail: %+v, %v", res, err)
	}
	res, err = r.Run(ctx, []string{"cat"}, strings.NewReader("hello stdin\n"))
	if err != nil || res.ExitCode != 0 || res.Stdout != "hello stdin\n" {
		t.Errorf("cat: %+v, %v", res, err)
	}
	res, err = r.Run(ctx, []string{"echoargs", "a b", "it's"}, nil)
	if err != nil || res.Stdout != `'a b' 'it'\''s'` {
		t.Errorf("echoargs: %+v, %v", res, err)
	}
	res, err = r.Run(ctx, []string{"big"}, nil)
	if err != nil || !strings.HasSuffix(res.Stdout, TruncatedMarker) || len(res.Stdout) != maxCapture+len(TruncatedMarker) {
		t.Errorf("big: len %d, %v", len(res.Stdout), err)
	}
}

func TestSSHRunnerContextCancel(t *testing.T) {
	r := dialTest(t, map[string]sshtest.Handler{
		"hang": func(_ string, _ io.Reader, _, _ io.Writer) int { time.Sleep(1 * time.Second); return 0 },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Run(ctx, []string{"hang"}, nil)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("cancel took %v", time.Since(start))
	}
}

// TestSSHRunnerSessionOpenHonorsContext: on a link that stopped answering the
// session open gets no reply; Run must still return when ctx ends (the
// broker token read's timeout depends on it).
func TestSSHRunnerSessionOpenHonorsContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	scfg := &ssh.ServerConfig{NoClientAuth: true}
	scfg.AddHostKey(signer)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		sc, _, reqs, err := ssh.NewServerConn(c, scfg)
		if err != nil {
			return
		}
		t.Cleanup(func() { sc.Close() })
		go ssh.DiscardRequests(reqs)
		// Channel opens are never read, so never answered.
	}()
	cl, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{User: "u", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (&SSHRunner{Client: cl}).Run(ctx, []string{"true"}, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the context's deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run ignored ctx while the session open got no answer")
	}
}
