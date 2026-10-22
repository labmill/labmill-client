//go:build desktop && !windows

package main

// startSplash: only Windows has the start splash (the app-starting cursor
// and the late window are a Windows problem); elsewhere it does nothing.
func startSplash(splashGeom) (closeSplash func()) {
	splashStarts.Add(1)
	return func() {}
}
