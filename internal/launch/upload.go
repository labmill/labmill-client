package launch

import (
	"context"
	"sync"

	"labmill-client/internal/account"
	"labmill-client/internal/store"
)

// startUploader sends the job reports and problem reports to the user's
// Labmill account in the background (internal/account). It does nothing while no
// account is linked (labmill login). A failed pass goes to labmill.log once
// while it repeats; the token is never in it. The returned func stops it and
// waits, so the store can close after it.
func startUploader(db *store.Store, words func(store.Job) []string, institution func(cluster string) string, ref func(cluster string) account.SystemRef, logLine func(string)) func() {
	path, err := account.Path()
	if err != nil || db == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		(&account.Uploader{Store: db, LinkPath: path, Institution: institution, Ref: ref, Words: words,
			OnError: func(err error) { logLine("job reports not sent: " + err.Error()) }}).Run(ctx)
	}()
	return func() { cancel(); wg.Wait() }
}
