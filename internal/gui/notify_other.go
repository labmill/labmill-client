//go:build !windows

package gui

import "runtime"

func platformNotify(title, text string) { unixNotify(runtime.GOOS, title, text) }
