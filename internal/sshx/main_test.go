package sshx

import (
	"errors"
	"net"
	"os"
	"testing"
)

// TestMain keeps the developer's own SSH agent out of the tests: its keys
// would be offered to the test servers (and count against their MaxAuthTries).
// Tests that want an agent set agentDial themselves.
func TestMain(m *testing.M) {
	agentDial = func() (net.Conn, error) { return nil, errors.New("no SSH agent in tests") }
	os.Exit(m.Run())
}
