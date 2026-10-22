//go:build windows

package startup

import (
	"time"

	"golang.org/x/sys/windows"
)

// processStart is the creation time of this process (Windows keeps it), so
// the time the loader and the Go package initializers took is visible.
func processStart() time.Time {
	var creation, exit, kernel, user windows.Filetime
	h, err := windows.GetCurrentProcess()
	if err == nil && windows.GetProcessTimes(h, &creation, &exit, &kernel, &user) == nil {
		if t := time.Unix(0, creation.Nanoseconds()); !t.After(time.Now()) && time.Since(t) < time.Hour {
			return t
		}
	}
	return time.Now()
}
