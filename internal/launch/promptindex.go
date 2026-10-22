package launch

import (
	"context"
	"sync"

	"labmill-client/internal/agent"
	"labmill-client/internal/app"
	"labmill-client/internal/knowledge"
	"labmill-client/internal/site"
)

// No duplicate knowledge (docs/dev/broker-knowledge.md): when the broker
// adds its index block to every request, the system prompt leaves out the
// skill list and the site facts the index already carries.
//
// The choice is made once per session, at the first Send, because the system
// block is cached and must stay byte-stable. The manifest is fetched in the
// background after startup; if its answer isn't there yet at the first Send,
// the full prompt is used for the whole session (simple and cache-safe).

// manifestRecorder wraps a knowledgeFetcher and remembers the manifest of
// the session's one successful Manifest call.
type manifestRecorder struct {
	knowledgeFetcher
	mu  sync.Mutex
	m   knowledge.Manifest
	got bool
}

func (r *manifestRecorder) Manifest(ctx context.Context) (knowledge.Manifest, error) {
	m, err := r.knowledgeFetcher.Manifest(ctx)
	if err == nil {
		r.mu.Lock()
		r.m, r.got = m, true
		r.mu.Unlock()
	}
	return m, err
}

// manifest returns the recorded manifest and whether there is one.
func (r *manifestRecorder) manifest() (knowledge.Manifest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m, r.got
}

// promptTrimmed is the decision rule: the prompt may leave out what the
// broker's index carries only when the broker said index:true for this
// site, its version equals the version this client loaded, and no override
// folder is in use (the user's own skills differ from the broker's).
func promptTrimmed(m knowledge.Manifest, have bool, st site.Site, base skillBase) bool {
	return have && m.Index && m.Site == st.Domain && base.Version != "" &&
		m.Version == base.Version && !base.Overridden
}

// choosePrompt sets ag.System for the session: the trimmed prompt when
// promptTrimmed holds for the recorded manifest (rec may be nil: no broker),
// the full one otherwise, also when the manifest hasn't arrived yet. It runs
// once, before the session's first model call.
func choosePrompt(ag *agent.Agent, env *app.Env, rec *manifestRecorder, st site.Site, base skillBase) {
	trimmed := false
	if rec != nil {
		m, have := rec.manifest()
		trimmed = promptTrimmed(m, have, st, base)
	}
	ag.System = app.SystemPromptFor(env, trimmed)
}

// withPromptChoice wraps the session's firstTurn hook (may be nil): it runs
// prev first (its dialogs may take a while, which gives the manifest more
// time to arrive), then chooses the system prompt.
func withPromptChoice(prev func(context.Context), ag *agent.Agent, env *app.Env, rec *manifestRecorder, st site.Site, base skillBase) func(context.Context) {
	return func(ctx context.Context) {
		if prev != nil {
			prev(ctx)
		}
		choosePrompt(ag, env, rec, st, base)
	}
}
