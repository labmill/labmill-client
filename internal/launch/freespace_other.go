//go:build !linux && !darwin && !windows

package launch

import "errors"

// freeSpace is unknown on this platform: the automatic download then skips
// the free-space check and keeps only its size limit.
func freeSpace(string) (int64, error) { return 0, errors.New("free space unknown") }
