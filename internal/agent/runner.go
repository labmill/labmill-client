package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"labmill-client/internal/broker"
)

// Send appends the user's message to history and runs the turn on the SDK's
// streaming tool runner (client.Beta.Messages.NewToolRunnerStreaming):
//
//   - Every model request is announced with EventStep StepWaitingForClaude
//     and carries broker.HintHeader: HintPlan for the first request of the
//     Send, HintRoutine after. The first request also asks for adaptive
//     thinking (unless NoThinking) and allows PlanMaxTokens; later ones
//     RoutineMaxTokens. A thinking block shows as EventStep StepThinking;
//     its text is never shown. Text deltas are reported as EventText.
//   - Prompt caching: a cache breakpoint on the fixed system prompt (System;
//     SystemExtra goes in a second, uncached block), one on the last tool
//     definition, and the top-level automatic breakpoint, which follows
//     the conversation (tool results such as loaded skill text included).
//   - Tools run one at a time in the order Claude listed them, although the
//     runner starts them together: unknown tool names get the runner's
//     "not found" error result (EventToolCall and EventToolResult, in
//     Claude's order among the other calls); for the rest EventToolCall, then for NeedsApproval tools Propose and
//     the Approver (waiting on ctx too, so a cancel never hangs on an open
//     dialog; a Propose that returns ErrNoApproval skips the Approver, even
//     a missing one, and Run gets the zero Decision); a decline returns "The user declined: <reason>" as a normal
//     result (EventDeclined, and EventToolResult marked as an error for the
//     front ends) and Run is not called; an approval emits EventApproved,
//     clears Decision.Edited unless the Proposal is Editable, sets
//     Decision.Proposal and checks ctx before Run. A Propose or Run error
//     becomes an error result ("Error: <tool>: <err>"); the loop goes on.
//     EventToolResult follows each tool. Propose and Run get Claude's input
//     bytes as sent (large numbers stay exact).
//   - An Approver error, a missing Approver (ErrNoApprover) or a canceled
//     ctx ends Send with that error (ctx.Err() for cancellation). If it
//     happened while this Send's latest reply's tool calls were being
//     answered, those calls get the results recorded so far, "not found"
//     for unknown tools, and error results "aborted" (or "canceled") with
//     EventToolResult for the rest, so the history stays API-valid.
//     Otherwise (e.g. a cancel during the Send's first request) the history
//     is returned as sent, earlier turns untouched.
//   - When a reply's prompt size reaches CompactAt, the runner compacts the
//     conversation before the next model turn (CompactBeforeNextTurn,
//     beta CompactionBeta; after the last reply it compacts and stops):
//     EventStep StepSummarizing, then EventNote NoteCompacted, and the
//     history becomes the compaction message plus what follows. A reply that
//     ends the turn with tool calls that never ran is not compacted. A
//     compaction stream cut off before message_stop is an error and leaves
//     the history as that request sent it.
//
// The turn ends when a reply asks for no tools: nil for end_turn and
// stop_sequence, ErrTruncated for max_tokens, ErrRefused for refusal. Tool
// calls in such a reply (e.g. cut off at max_tokens) are not run; they get
// error results "not run: response ended with <reason>". After MaxTurns
// model requests still asking for tools, the turn ends without an error:
// the last reply's tool calls get "not run: too many tool-use turns" results,
// and an assistant message (MaxTurnsMessage) is added to the history and
// emitted as text, so the history stays valid and the user's next message
// ("continue") carries on. Empty
// text blocks and empty assistant messages never enter the history (the
// API rejects them). A stream that ends without message_stop is an error;
// the partial reply is dropped.
//
// For Origin Voice, VoiceNote is prepended to the text. Send returns the
// updated history even on error, so the conversation can continue.
func (a *Agent) Send(ctx context.Context, history []anthropic.BetaMessageParam, text string, origin Origin) ([]anthropic.BetaMessageParam, error) {
	if origin == Voice {
		text = VoiceNote + text
	}
	history = append(history[:len(history):len(history)], anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(text)))
	if err := ctx.Err(); err != nil {
		return history, err
	}
	maxTurns := a.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 25
	}
	compactAt := a.CompactAt
	if compactAt == 0 {
		compactAt = DefaultCompactAt
	}
	svc, opts, err := a.Model.BetaMessages()
	if err != nil {
		return history, fmt.Errorf("agent: model: %w", err)
	}

	var system []anthropic.BetaTextBlockParam
	if a.System != "" {
		system = append(system, anthropic.BetaTextBlockParam{Text: a.System, CacheControl: anthropic.NewBetaCacheControlEphemeralParam()})
	}
	if a.SystemExtra != nil {
		if extra := a.SystemExtra(); extra != "" {
			system = append(system, anthropic.BetaTextBlockParam{Text: extra})
		}
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	t := &turn{a: a, cancel: cancelRun, results: map[string]toolResult{}}
	var tools []anthropic.BetaTool
	if a.Tools != nil {
		for _, name := range a.Tools.order {
			tools = append(tools, &betaTool{t: t, tool: a.Tools.tools[name]})
		}
	}
	params := anthropic.BetaToolRunnerParams{
		BetaMessageNewParams: anthropic.BetaMessageNewParams{
			Model:        anthropic.Model(broker.ModelPlaceholder),
			MaxTokens:    RoutineMaxTokens,
			Messages:     history,
			System:       system,
			Betas:        []anthropic.AnthropicBeta{CompactionBeta},
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		},
		MaxIterations: maxTurns,
	}
	if !a.NoThinking {
		params.MaxTokens = PlanMaxTokens
		params.Thinking = anthropic.BetaThinkingConfigParamUnion{OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{
			Display: anthropic.BetaThinkingConfigAdaptiveDisplayOmitted,
		}}
	}
	var runner *anthropic.BetaToolRunnerStreaming
	// First, so it is the outermost middleware: the broker's 401 retry
	// happens inside it and is not counted as a second request.
	ropts := append([]option.RequestOption{option.WithMiddleware(t.middleware(&runner))}, opts...)
	runner = svc.NewToolRunnerStreaming(tools, params, ropts...)
	if n := len(runner.Params.Tools); n > 0 && runner.Params.Tools[n-1].OfTool != nil {
		runner.Params.Tools[n-1].OfTool.CacheControl = anthropic.NewBetaCacheControlEphemeralParam()
	}

	var lastReply *anthropic.BetaMessage
	planned := false
	for !runner.IsCompleted() {
		if last := runner.LastMessage(); last != nil && last.StopReason == anthropic.BetaStopReasonToolUse && runner.IterationCount() < maxTurns {
			t.prepare(last)
		}
		gotEvents, stopped, turnErr := t.stream(runCtx, runner)
		if turnErr != nil {
			return t.abort(ctx, runner, turnErr)
		}
		if !gotEvents {
			break // the runner had nothing left to send
		}
		if t.compacting {
			if !stopped {
				// The runner may have taken a partial compaction block as the
				// new history; keep the conversation as it was sent.
				return t.preCompact, errors.New("agent: model: broker: stream ended before message_stop")
			}
			if m := runner.LastMessage(); m != nil && m.StopReason == anthropic.BetaStopReasonCompaction && hasSummary(m) {
				if a.OnReply != nil {
					a.OnReply(m)
				}
				a.emit(Event{Kind: EventNote, Text: NoteCompacted})
			}
			continue
		}
		if !stopped {
			dropLastAssistant(runner)
			return runner.Messages(), errors.New("agent: model: broker: stream ended before message_stop")
		}
		lastReply = runner.LastMessage()
		if a.OnReply != nil {
			a.OnReply(lastReply)
		}
		sanitizeLast(runner)
		if !planned {
			planned = true
			runner.Params.Thinking = anthropic.BetaThinkingConfigParamUnion{}
			runner.Params.MaxTokens = RoutineMaxTokens
		}
		// A reply that ends the turn with tool calls that never run (e.g. cut
		// off at max_tokens) can't be compacted; the runner would only warn.
		cutOff := lastReply.StopReason != anthropic.BetaStopReasonToolUse && len(toolUses(lastReply)) > 0
		if u := lastReply.Usage; compactAt > 0 && !cutOff && u.InputTokens+u.CacheReadInputTokens+u.CacheCreationInputTokens >= compactAt {
			runner.CompactBeforeNextTurn(anthropic.BetaCompactionConfigUnionParam{})
		}
	}

	history = runner.Messages()
	if lastReply == nil {
		return history, nil
	}
	uses := toolUses(lastReply)
	if lastReply.StopReason == anthropic.BetaStopReasonToolUse {
		history = a.notRun(history, uses, "not run: too many tool-use turns")
		msg := MaxTurnsMessage(maxTurns)
		a.emit(Event{Kind: EventText, Text: "\n\n" + msg})
		return append(history[:len(history):len(history)], anthropic.BetaMessageParam{Role: anthropic.BetaMessageParamRoleAssistant, Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(msg)}}), nil
	}
	stopErr := stopError(lastReply.StopReason)
	if len(uses) > 0 {
		history = a.notRun(history, uses, fmt.Sprintf("not run: response ended with %s", lastReply.StopReason))
	}
	return history, stopErr
}

// turn is the state of one Send shared by the loop, the request middleware
// and the tool handlers.
type turn struct {
	a      *Agent
	cancel context.CancelFunc // cancels the runner's context (fatal errors)

	// compacting is set by the middleware for each request: whether it is
	// a compaction request. Read by the loop after the request's events.
	compacting bool
	// preCompact is the history as a compaction request sent it (set by
	// the middleware), returned when that request's stream is cut off.
	preCompact []anthropic.BetaMessageParam

	execMu sync.Mutex // one tool at a time

	mu       sync.Mutex
	prepared *anthropic.BetaMessage // the reply whose tool calls queue holds
	queue    []*queued
	trailing []anthropic.BetaToolUseBlock // unknown-tool calls after the last known one
	results  map[string]toolResult        // by tool_use ID, for the reply in prepared
	emitted  map[string]bool              // unknown-tool calls already reported
	fatal    error
}

// queued is one tool call of the current reply, in Claude's order.
type queued struct {
	id, name, input string                       // input as the runner passes it (json.Marshal of the decoded input)
	raw             json.RawMessage              // Claude's input bytes, passed to Run
	before          []anthropic.BetaToolUseBlock // unknown-tool calls just before this one
	claimed         bool
	prev            <-chan struct{} // closed when the previous call is done
	done            chan struct{}   // closed when this call is done
}

// notFound is the runner's result text for a call to an unknown tool.
func notFound(name string) string { return fmt.Sprintf("Error: Tool '%s' not found", name) }

// toolResult is what the runner sent back for a tool call.
type toolResult struct {
	text  string
	isErr bool
}

// middleware returns the request middleware that tells the planning
// request from routine and compaction requests (the runner counts no
// iteration for a compaction request), emits the matching step and sets
// the hint header.
func (t *turn) middleware(runner **anthropic.BetaToolRunnerStreaming) option.Middleware {
	lastIter := 0
	return func(r *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		n := (*runner).IterationCount()
		hint := broker.HintRoutine
		if n == lastIter {
			t.compacting = true
			t.preCompact = (*runner).Messages()
			t.a.emit(Event{Kind: EventStep, Text: StepSummarizing})
		} else {
			lastIter = n
			t.compacting = false
			if n == 1 {
				hint = broker.HintPlan
			}
			t.a.emit(Event{Kind: EventStep, Text: StepWaitingForClaude})
		}
		r.Header.Set(broker.HintHeader, string(hint))
		return next(r)
	}
}

// stream runs one NextStreaming step, forwarding its events to the UI. It
// reports whether any event arrived (none: the runner is done) and whether
// message_stop did. A panic in the SDK (e.g. converting an unknown content
// block) becomes an error.
func (t *turn) stream(ctx context.Context, runner *anthropic.BetaToolRunnerStreaming) (gotEvents, stopped bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("agent: cannot handle the model reply: %v", r)
		}
	}()
	for ev, serr := range runner.NextStreaming(ctx) {
		if serr != nil {
			return gotEvents, stopped, serr
		}
		gotEvents = true
		switch ev.Type {
		case "content_block_start":
			if typ := ev.ContentBlock.Type; typ == "thinking" || typ == "redacted_thinking" {
				t.a.emit(Event{Kind: EventStep, Text: StepThinking})
			}
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				t.a.emit(Event{Kind: EventText, Text: ev.Delta.Text})
			}
		case "message_stop":
			stopped = true
		}
	}
	return gotEvents, stopped, nil
}

// prepare queues the tool calls of reply (once per reply), in order, before
// the runner executes them. Calls to unknown tools (the runner answers them
// itself) are reported in Claude's order: before the next known call runs,
// after the last one, or right away when there is none.
func (t *turn) prepare(reply *anthropic.BetaMessage) {
	t.mu.Lock()
	if t.prepared == reply {
		t.mu.Unlock()
		return
	}
	t.prepared = reply
	t.queue = nil
	t.trailing = nil
	t.results = map[string]toolResult{}
	t.emitted = map[string]bool{}
	prev := make(chan struct{})
	close(prev)
	var unknown []anthropic.BetaToolUseBlock
	for _, b := range reply.Content {
		if b.Type != "tool_use" {
			continue
		}
		u := b.AsToolUse()
		if !t.a.known(u.Name) {
			unknown = append(unknown, u)
			continue
		}
		q := &queued{id: u.ID, name: u.Name, input: runnerInput(u.Input), raw: b.Input, before: unknown,
			prev: prev, done: make(chan struct{})}
		unknown = nil
		prev = q.done
		t.queue = append(t.queue, q)
	}
	t.trailing = unknown
	none := len(t.queue) == 0
	t.mu.Unlock()
	if none {
		t.emitUnknown(unknown)
	}
}

// emitUnknown reports calls to unknown tools (once each) with the runner's
// "not found" result.
func (t *turn) emitUnknown(uses []anthropic.BetaToolUseBlock) {
	for _, u := range uses {
		t.mu.Lock()
		done := t.emitted[u.ID]
		t.emitted[u.ID] = true
		t.mu.Unlock()
		if done {
			continue
		}
		t.a.emit(Event{Kind: EventToolCall, Tool: u.Name, Input: nonEmpty(json.RawMessage(runnerInput(u.Input)))})
		t.a.emit(Event{Kind: EventToolResult, Tool: u.Name, Text: notFound(u.Name), IsError: true})
	}
}

// known reports whether name is a registered tool.
func (a *Agent) known(name string) bool {
	if a.Tools == nil {
		return false
	}
	_, ok := a.Tools.Get(name)
	return ok
}

// claim returns the first unclaimed queued call with this name and input.
func (t *turn) claim(name string, input json.RawMessage) *queued {
	key := runnerInput(input)
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, q := range t.queue {
		if !q.claimed && q.name == name && q.input == key {
			q.claimed = true
			return q
		}
	}
	return nil
}

func (t *turn) record(id string, r toolResult) {
	t.mu.Lock()
	t.results[id] = r
	t.mu.Unlock()
}

func (t *turn) setFatal(err error) {
	t.mu.Lock()
	if t.fatal == nil {
		t.fatal = err
	}
	t.mu.Unlock()
	t.cancel()
}

// abort ends Send after err stopped the runner. For an approver failure or
// a cancellation while this Send's latest reply's tool calls were being
// answered (the history's last assistant message is the reply in prepared,
// followed by nothing or the runner's results for it), those calls are
// answered with the results recorded so far, "not found" for unknown tools
// and "aborted"/"canceled" for the rest. Otherwise (e.g. a cancel during the
// first request of the Send) the history is returned as the runner has it.
func (t *turn) abort(ctx context.Context, runner *anthropic.BetaToolRunnerStreaming, err error) ([]anthropic.BetaMessageParam, error) {
	history := runner.Messages()
	t.mu.Lock()
	fatal := t.fatal
	results := t.results
	prepared := t.prepared
	t.mu.Unlock()
	note := "aborted"
	switch {
	case ctx.Err() != nil:
		note, fatal = "canceled", ctx.Err()
	case fatal == nil:
		return history, fmt.Errorf("agent: model: %w", err)
	}
	if prepared == nil {
		return history, fatal
	}
	i := len(history) - 1
	for i >= 0 && history[i].Role != anthropic.BetaMessageParamRoleAssistant {
		i--
	}
	if i < 0 || i < len(history)-2 {
		return history, fatal
	}
	uses := paramToolUses(history[i])
	want := toolUses(prepared)
	if len(uses) == 0 || len(uses) != len(want) {
		return history, fatal
	}
	for k := range uses {
		if uses[k].id != want[k].ID {
			return history, fatal
		}
	}
	if i == len(history)-2 && !answers(history[i+1], uses) {
		return history, fatal
	}
	history = history[: i+1 : i+1]
	blocks := make([]anthropic.BetaContentBlockParamUnion, 0, len(uses))
	for k, u := range uses {
		if r, ok := results[u.id]; ok {
			blocks = append(blocks, anthropic.NewBetaToolResultBlock(u.id, r.text, r.isErr))
			continue
		}
		if !t.a.known(u.name) {
			t.emitUnknown(want[k : k+1])
			blocks = append(blocks, anthropic.NewBetaToolResultBlock(u.id, notFound(u.name), true))
			continue
		}
		t.a.emit(Event{Kind: EventToolResult, Tool: u.name, Text: note, IsError: true})
		blocks = append(blocks, anthropic.NewBetaToolResultBlock(u.id, note, true))
	}
	return append(history, anthropic.NewBetaUserMessage(blocks...)), fatal
}

// answers reports whether m is a user message of tool results for exactly
// the calls in uses.
func answers(m anthropic.BetaMessageParam, uses []useRef) bool {
	if m.Role != anthropic.BetaMessageParamRoleUser {
		return false
	}
	ids := map[string]bool{}
	for _, b := range m.Content {
		if b.OfToolResult == nil {
			return false
		}
		ids[b.OfToolResult.ToolUseID] = true
	}
	if len(ids) != len(uses) {
		return false
	}
	for _, u := range uses {
		if !ids[u.id] {
			return false
		}
	}
	return true
}

// notRun answers uses with error results text when the history ends with
// the reply that holds them.
func (a *Agent) notRun(history []anthropic.BetaMessageParam, uses []anthropic.BetaToolUseBlock, text string) []anthropic.BetaMessageParam {
	if len(uses) == 0 || len(history) == 0 || history[len(history)-1].Role != anthropic.BetaMessageParamRoleAssistant {
		return history
	}
	blocks := make([]anthropic.BetaContentBlockParamUnion, 0, len(uses))
	for _, u := range uses {
		blocks = append(blocks, anthropic.NewBetaToolResultBlock(u.ID, text, true))
	}
	return append(history[:len(history):len(history)], anthropic.NewBetaUserMessage(blocks...))
}

// betaTool adapts a Tool to the runner. Execute keeps the runner's
// concurrent calls in Claude's order and one at a time.
type betaTool struct {
	t    *turn
	tool Tool
}

func (b *betaTool) Name() string        { return b.tool.Name }
func (b *betaTool) Description() string { return b.tool.Description }
func (b *betaTool) InputSchema() anthropic.BetaToolInputSchemaParam {
	s := b.tool.Schema
	return anthropic.BetaToolInputSchemaParam{Properties: s.Properties, Required: s.Required, ExtraFields: s.ExtraFields}
}

// Execute implements anthropic.BetaTool. A returned error becomes an error
// result "Error: <err>"; a canceled call returns ctx.Err() and is answered
// by abort instead.
func (b *betaTool) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	t := b.t
	q := t.claim(b.tool.Name, input)
	if q != nil {
		defer close(q.done)
		select {
		case <-q.prev:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	t.execMu.Lock()
	defer t.execMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Claude's own bytes, not the runner's re-encoding of a decoded value
	// (which can round large numbers).
	raw := input
	if q != nil {
		t.emitUnknown(q.before)
		if len(q.raw) > 0 && json.Valid(q.raw) {
			raw = q.raw
		}
		defer func() {
			t.mu.Lock()
			last := len(t.queue) > 0 && t.queue[len(t.queue)-1] == q
			trailing := t.trailing
			t.mu.Unlock()
			if last {
				t.emitUnknown(trailing)
			}
		}()
	}
	out, isErr, fatal := t.a.runTool(ctx, b.tool, nonEmpty(raw))
	if fatal != nil {
		t.setFatal(fatal)
		return nil, fatal
	}
	if isErr {
		out = "Error: " + out // the runner's wording for an error result
	}
	if q != nil {
		t.record(q.id, toolResult{text: out, isErr: isErr})
	}
	if isErr {
		return nil, errors.New(out[len("Error: "):])
	}
	return []anthropic.BetaToolResultBlockParamContentUnion{{OfText: &anthropic.BetaTextBlockParam{Text: out}}}, nil
}

// runTool runs one tool call with its approval. The returned error is fatal
// to Send (approver failure, no approver, cancellation); tool failures come
// back as isErr with the text instead.
func (a *Agent) runTool(ctx context.Context, tool Tool, input json.RawMessage) (out string, isErr bool, fatal error) {
	a.emit(Event{Kind: EventToolCall, Tool: tool.Name, Input: input})
	var d Decision
	gated := tool.NeedsApproval
	var p Proposal
	var perr error
	if gated {
		p, perr = tool.Propose(input)
		gated = !errors.Is(perr, ErrNoApproval) // this call needs no approval
	}
	if gated {
		if a.Approver == nil {
			return "", false, ErrNoApprover
		}
		if err := perr; err != nil {
			out = fmt.Sprintf("%s: %v", tool.Name, err)
			a.emit(Event{Kind: EventToolResult, Tool: tool.Name, Text: out, IsError: true})
			return out, true, nil
		}
		var err error
		d, err = a.approve(ctx, p)
		if err != nil {
			return "", false, err
		}
		if !d.Approved {
			reason := d.Reason
			if reason == "" {
				reason = "no reason given"
			}
			out = "The user declined: " + reason
			a.emit(Event{Kind: EventDeclined, Tool: tool.Name, Text: reason})
			a.emit(Event{Kind: EventToolResult, Tool: tool.Name, Text: out, IsError: true})
			return out, false, nil
		}
		if !p.Editable {
			d.Edited = ""
		}
		d.Proposal = p
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		a.emit(Event{Kind: EventApproved, Tool: tool.Name})
	}
	res, err := tool.Run(ctx, input, d)
	if err != nil {
		out = fmt.Sprintf("%s: %v", tool.Name, err)
		a.emit(Event{Kind: EventToolResult, Tool: tool.Name, Text: out, IsError: true})
		return out, true, nil
	}
	if d.Auto && !strings.HasPrefix(res, AutoRanPrefix) {
		res = AutoApprovedNote + "\n" + res
	}
	a.emit(Event{Kind: EventToolResult, Tool: tool.Name, Text: res})
	return res, false, nil
}

// approve asks the Approver, but stops waiting when ctx ends (the dialog
// may stay open; the front end dismisses it when the turn ends).
func (a *Agent) approve(ctx context.Context, p Proposal) (Decision, error) {
	type answer struct {
		d   Decision
		err error
	}
	ch := make(chan answer, 1)
	go func() {
		d, err := a.Approver.Approve(ctx, p)
		ch <- answer{d, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return r.d, fmt.Errorf("agent: approval: %w", r.err)
		}
		return r.d, nil
	case <-ctx.Done():
		return Decision{}, ctx.Err()
	}
}

// runnerInput is a tool_use block's input as the runner hands it to
// Execute: json.Marshal of the decoded value (compact, sorted keys, HTML
// characters escaped). Marshaling that again as a json.RawMessage gives the
// same text, so claim can apply it to what Execute received.
func runnerInput(input any) string {
	b, err := json.Marshal(input)
	if err != nil {
		return fmt.Sprint(input)
	}
	return string(b)
}

func nonEmpty(input json.RawMessage) json.RawMessage {
	if len(input) == 0 || string(input) == "null" {
		return json.RawMessage("{}")
	}
	return input
}

func toolUses(m *anthropic.BetaMessage) []anthropic.BetaToolUseBlock {
	var out []anthropic.BetaToolUseBlock
	for _, b := range m.Content {
		if b.Type == "tool_use" {
			out = append(out, b.AsToolUse())
		}
	}
	return out
}

type useRef struct{ id, name string }

func paramToolUses(m anthropic.BetaMessageParam) []useRef {
	var out []useRef
	for _, b := range m.Content {
		if b.OfToolUse != nil {
			out = append(out, useRef{b.OfToolUse.ID, b.OfToolUse.Name})
		}
	}
	return out
}

func hasSummary(m *anthropic.BetaMessage) bool {
	for _, b := range m.Content {
		if b.Type == "compaction" && b.Content.OfString != "" {
			return true
		}
	}
	return false
}

// sanitizeLast drops empty text blocks from the reply the runner just
// appended, and the reply itself when nothing is left.
func sanitizeLast(runner *anthropic.BetaToolRunnerStreaming) {
	msgs := runner.Params.Messages
	n := len(msgs)
	if n == 0 || msgs[n-1].Role != anthropic.BetaMessageParamRoleAssistant {
		return
	}
	msgs[n-1].Content = dropEmptyText(msgs[n-1].Content)
	if len(msgs[n-1].Content) == 0 {
		runner.Params.Messages = msgs[:n-1]
	}
}

// dropLastAssistant removes a partial reply the runner appended.
func dropLastAssistant(runner *anthropic.BetaToolRunnerStreaming) {
	msgs := runner.Params.Messages
	if n := len(msgs); n > 0 && msgs[n-1].Role == anthropic.BetaMessageParamRoleAssistant {
		runner.Params.Messages = msgs[:n-1]
	}
}

// dropEmptyText removes text blocks whose text is empty.
func dropEmptyText(blocks []anthropic.BetaContentBlockParamUnion) []anthropic.BetaContentBlockParamUnion {
	out := blocks[:0:0]
	for _, b := range blocks {
		if b.OfText != nil && b.OfText.Text == "" {
			continue
		}
		out = append(out, b)
	}
	return out
}

// stopError maps a stop reason that requested no tools to Send's result.
func stopError(r anthropic.BetaStopReason) error {
	switch r {
	case anthropic.BetaStopReasonMaxTokens:
		return ErrTruncated
	case anthropic.BetaStopReasonRefusal:
		return ErrRefused
	}
	return nil
}

func (a *Agent) emit(e Event) {
	if a.OnEvent != nil {
		a.OnEvent(e)
	}
}
