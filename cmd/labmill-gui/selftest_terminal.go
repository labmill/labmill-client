//go:build desktop

package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"labmill-client/internal/sshx"
)

// The terminal part of the self-test (M62.2): a fake login node. The shell
// greets with the cluster and folder it was opened for, echoes each typed
// line, reports the terminal size when it gets a resize and exits with
// status 5 on "exit". Nothing reaches a network.

var (
	selfTermMu      sync.Mutex
	selfTermSystem  []string // clusters "Open in system terminal" was asked for
	selfTermRunning int      // fake shells running now
)

// terminalClusters are the clusters the fake page can open (the same name
// the self-test jobs use, so "Open terminal here" resolves).
func (t *selfTest) terminalClusters() []TerminalCluster {
	return []TerminalCluster{{Name: "selftest-cluster", Host: "selftest-login"}, {Name: "selftest-gpu", Host: "selftest-gpu-login"}}
}

// openSystemTerminal records the request instead of starting a program.
func (t *selfTest) openSystemTerminal(cli, cluster string) error {
	selfTermMu.Lock()
	defer selfTermMu.Unlock()
	selfTermSystem = append(selfTermSystem, cluster)
	return nil
}

// terminalRunner is the fake login node (gui.TerminalRunner).
func (t *selfTest) terminalRunner(ctx context.Context, cluster string, opts sshx.ShellOptions, connected func(), allowSignIn bool) (int, error) {
	selfTermMu.Lock()
	selfTermRunning++
	selfTermMu.Unlock()
	defer func() {
		selfTermMu.Lock()
		selfTermRunning--
		selfTermMu.Unlock()
	}()
	connected()
	dir := opts.Dir
	if dir == "" {
		dir = "~"
	}
	fmt.Fprintf(opts.Stdout, "\x1b[32mselftest-shell\x1b[0m on %s in %s (%dx%d)\r\n$ ", cluster, dir, opts.Width, opts.Height)
	go func() {
		for {
			select {
			case sz, ok := <-opts.Resize:
				if !ok {
					return
				}
				fmt.Fprintf(opts.Stdout, "\r\nsize %dx%d\r\n$ ", sz[0], sz[1])
			case <-ctx.Done():
				return
			}
		}
	}()
	var line strings.Builder
	buf := make([]byte, 256)
	for {
		n, err := opts.Stdin.Read(buf)
		for _, c := range buf[:n] {
			if c != '\r' {
				line.WriteByte(c)
				continue
			}
			text := line.String()
			line.Reset()
			if text == "exit" {
				io.WriteString(opts.Stdout, "\r\nbye\r\n")
				return 5, nil
			}
			fmt.Fprintf(opts.Stdout, "\r\ngot:%s\r\n$ ", text)
		}
		if err != nil || ctx.Err() != nil {
			return 0, ctx.Err()
		}
	}
}
