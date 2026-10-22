//go:build windows

package skill

// openFlags are added to O_RDONLY when opening skill files; Windows has no
// FIFOs to worry about.
const openFlags = 0
