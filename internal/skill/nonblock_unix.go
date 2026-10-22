//go:build !windows

package skill

import "syscall"

// openFlags are added to O_RDONLY when opening skill files: O_NONBLOCK makes
// opening a FIFO return at once instead of waiting for a writer, so a file
// swapped for a FIFO after the type check can't hang startup.
const openFlags = syscall.O_NONBLOCK
