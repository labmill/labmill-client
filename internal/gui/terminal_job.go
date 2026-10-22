package gui

import (
	"context"
	"fmt"
	"strings"

	"labmill-client/internal/store"
)

// jobHasTerminal says whether "Open terminal here" is offered for j: it was
// submitted and has a folder on the cluster.
func jobHasTerminal(j store.Job) bool {
	return j.RemoteID != "" && j.RemoteDir != "" && !strings.ContainsAny(j.Cluster+j.RemoteDir, "\n\r\x00")
}

// Busy reports whether a turn (a reply or a Settings action) is running.
func (a *App) Busy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// JobTerminalTarget returns the cluster and remote folder of job id: where
// "Open terminal here" starts. The page passes only the job's ID, so it can't
// name another folder.
func (a *App) JobTerminalTarget(id int64) (cluster, dir string, err error) {
	if a.jobs == nil {
		return "", "", fmt.Errorf("there's no job #%d", id)
	}
	js, err := a.jobs(context.Background())
	if err != nil {
		return "", "", fmt.Errorf("listing jobs: %w", err)
	}
	for _, j := range js {
		if j.ID != id {
			continue
		}
		if !jobHasTerminal(j) {
			return "", "", fmt.Errorf("job #%d has no folder on the cluster", id)
		}
		return j.Cluster, j.RemoteDir, nil
	}
	return "", "", fmt.Errorf("there's no job #%d", id)
}
