//go:build desktop && darwin

package main

import (
	"context"

	wr "github.com/wailsapp/wails/v2/pkg/runtime"
)

// windowSetPos: Wails reads and sets the position relative to the current
// screen's visible frame on macOS, so the same call restores what it saved.
func windowSetPos(ctx context.Context, x, y int) { wr.WindowSetPosition(ctx, x, y) }
