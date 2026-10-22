package sshx

import (
	"context"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// agentDial opens a connection to the user's SSH agent: the socket named by
// SSH_AUTH_SOCK, and on Windows also the OpenSSH agent's named pipe (see
// agent_windows.go). It returns an error when there is no agent. Tests
// replace it.
var agentDial = defaultAgentDial

// defaultAgentDial is agentDial's default: SSH_AUTH_SOCK as a Unix socket.
func defaultAgentDial() (net.Conn, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" || strings.HasPrefix(sock, `\\.\pipe\`) {
		return platformAgentDial(sock)
	}
	return net.DialTimeout("unix", sock, 2*time.Second)
}

// agentListTimeout bounds asking the agent for its keys: a stuck agent (a
// locked key store waiting for a prompt, a dead forwarded socket) must not
// hang the login.
const agentListTimeout = 3 * time.Second

// agentSigners returns the signers of the keys the SSH agent holds and a
// function that closes the agent connection (call it once the login has
// finished: signing happens during the handshake). With no agent, or an
// agent that can't be asked within agentListTimeout or before ctx ends, it
// returns no signers and a no-op close.
func agentSigners(ctx context.Context) ([]ssh.Signer, func()) {
	conn, err := agentDial()
	if err != nil || conn == nil {
		return nil, func() {}
	}
	// Closing the connection on ctx.Done unblocks a stuck Signers() call.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(agentListTimeout))
	sigs, err := agent.NewClient(conn).Signers()
	_ = conn.SetDeadline(time.Time{})
	if ctx.Err() != nil || err != nil || len(sigs) == 0 {
		stop()
		conn.Close()
		return nil, func() {}
	}
	// The close-on-cancel stays active: a signature the agent never answers
	// (a per-use confirmation nobody approves) blocks inside the handshake,
	// and only closing the agent connection when ctx ends frees it.
	return sigs, func() { stop(); conn.Close() }
}
