//go:build !windows

package sshx

import (
	"errors"
	"net"
)

// platformAgentDial is the agent connection when SSH_AUTH_SOCK is unset or
// names a Windows pipe: there is none on Unix systems.
func platformAgentDial(string) (net.Conn, error) {
	return nil, errors.New("no SSH agent (SSH_AUTH_SOCK is not set)")
}
