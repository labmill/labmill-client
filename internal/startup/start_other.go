//go:build !windows

package startup

import "time"

func processStart() time.Time { return time.Now() }
