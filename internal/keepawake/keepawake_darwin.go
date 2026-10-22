//go:build darwin

package keepawake

import (
	"os"
	"os/exec"
	"strconv"
)

// osStart runs caffeinate -i (no idle sleep) for as long as this process lives.
func osStart() func() {
	cmd := exec.Command("caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return func() {}
	}
	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}
