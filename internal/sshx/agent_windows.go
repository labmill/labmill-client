//go:build windows

package sshx

import (
	"context"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

// openSSHAgentPipe is the named pipe of Windows' OpenSSH Authentication Agent
// service (the ssh-agent that "ssh-add" talks to).
const openSSHAgentPipe = `\\.\pipe\openssh-ssh-agent`

// platformAgentDial is the agent connection when SSH_AUTH_SOCK is unset or
// names a pipe: the named pipe sock, else the Windows OpenSSH agent's pipe.
func platformAgentDial(sock string) (net.Conn, error) {
	if sock == "" {
		sock = openSSHAgentPipe
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return winio.DialPipeContext(ctx, sock)
}
