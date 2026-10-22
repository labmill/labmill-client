//go:build desktop && windows

package main

import "golang.org/x/sys/windows"

// otherInstanceRunning reports whether another process already holds the
// single-instance mutex that Wails creates for uniqueID (internal
// frontend/desktop/windows/single_instance.go: "wails-app-"+id+"sim").
// Wails exits the second process inside frontend.Run, so a tray started
// before that would be left behind as a ghost icon.
func otherInstanceRunning(uniqueID string) bool {
	name, err := windows.UTF16PtrFromString("wails-app-" + uniqueID + "sim")
	if err != nil {
		return false
	}
	h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, name)
	if err != nil {
		return false
	}
	windows.CloseHandle(h)
	return true
}
