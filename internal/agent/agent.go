// Package agent runs Labmill's tool-use loop: it sends the conversation to
// Claude through the broker, executes the typed tools Claude asks for, and
// loops until Claude stops asking.
//
// There is no arbitrary shell. Every action is a Tool with a fixed JSON input
// schema. A tool that moves data, submits, cancels or deletes anything sets
// NeedsApproval, and the loop shows the user a Proposal and waits for an
// explicit Decision before running it. Nothing leaves the machine without
// that approval.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"labmill-client/internal/skill"
)

// Model is what the loop needs from the broker: the SDK's Beta Messages
// service the tool runner runs on, and the request options every call must
// carry (authentication). *broker.Client implements it; tests point one at
// a fake broker (agenttest). A non-nil error means there is no usable
// broker; Send then fails with it before anything is sent.
type Model interface {
	BetaMessages() (*anthropic.BetaMessageService, []option.RequestOption, error)
}

// Proposal is what the user sees before an approval-gated tool runs.
type Proposal struct {
	Tool     string // tool name
	Summary  string // one line, e.g. "Upload 14,212 files (86 GB) to bigiron:/scratch/alice/mri"
	Details  string // full content to review: file list, generated sbatch script, ...
	Editable bool   // the user may edit Details (e.g. a generated script) before approving
	// Secret marks a proposal that would send the content of a sign-in
	// secret file to Claude. "Approve everything automatically" never
	// approves it: the user always decides (M43.2).
	Secret bool
}

// Decision is the user's answer to a Proposal.
type Decision struct {
	Approved bool
	// Edited is the replacement Details when the Proposal was Editable and the
	// user edited it; "" means unchanged. The loop clears it for non-editable
	// proposals, so a tool can never run something other than what was shown.
	Edited string
	Reason string // optional, e.g. why the user declined; passed back to Claude
	// Auto is set by AutoApprover: the setting "Approve everything
	// automatically" approved this, nobody looked at it. Tools that keep
	// safeguards for unreviewed runs (remote_command redacts output) read it,
	// and the loop tells Claude in the result.
	Auto bool
	// Proposal is the exact Proposal the user approved, filled in by the loop,
	// so Run can act on what was shown rather than re-deriving it.
	Proposal Proposal
}

// AutoRanPrefix starts the note a tool puts first in the result of a step
// that ran without anyone's approval; the loop adds AutoApprovedNote only
// when the result doesn't already start with it.
const AutoRanPrefix = "(ran without approval"

// AutoApprovedNote is added to the result of a step that the setting
// "Approve everything automatically" approved, so Claude knows nobody
// reviewed it.
const AutoApprovedNote = "(approved automatically by the user's setting; nobody reviewed it)"

// AutoApprover wraps an Approver for "Approve everything automatically"
// (M43.2): while On returns true, a Proposal is approved at once and
// unedited, as if the user had clicked Approve, and Note is told which one.
// Proposals marked Secret always go to Next (the user decides), and so does
// everything while On is nil or false. On is read per proposal, so a change
// applies at once. It is the one place that covers the GUI, the TUI and the
// tests; Choosers and logins are separate interfaces and never pass here.
type AutoApprover struct {
	Next Approver
	On   func() bool
	Note func(Proposal) // optional: called for each automatic approval
}

// Approve implements Approver.
func (a *AutoApprover) Approve(ctx context.Context, p Proposal) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if !p.Secret && a.On != nil && a.On() {
		if a.Note != nil {
			a.Note(p)
		}
		return Decision{Approved: true, Auto: true}, nil
	}
	if a.Next == nil {
		return Decision{}, ErrNoApprover
	}
	return a.Next.Approve(ctx, p)
}

// Approver asks the user. The TUI implements it with a Huh form; headless
// mode and tests use other implementations.
type Approver interface {
	Approve(ctx context.Context, p Proposal) (Decision, error)
}

// ChoiceOption is one answer the user can pick in a Choice.
type ChoiceOption struct {
	Label       string // short, one line, e.g. "Use a module"
	Description string // optional explanation shown under the label
}

// Choice is a question with a few fixed answers, shown as a selection dialog
// (the ask_user tool). Picking an answer changes nothing by itself, so a
// Choice is never an approval: actions keep their own Proposal.
type Choice struct {
	Question   string
	Options    []ChoiceOption
	AllowOther bool // the user may type their own answer instead
}

// ChoiceAnswer is the user's answer to a Choice. Read it in this order:
// Skipped (the user dismissed the question, or the turn ended), else Other
// (non-empty: the user typed their own answer), else Index (the chosen
// option, 0-based).
type ChoiceAnswer struct {
	Index   int
	Other   string
	Skipped bool
}

// Chooser asks the user a Choice. Front ends show it as a selection dialog
// and send exactly one answer per question; a dialog that is dismissed, or
// still open when the turn ends, answers Skipped.
type Chooser interface {
	Choose(ctx context.Context, q Choice) (ChoiceAnswer, error)
}

// Tool is one typed action Claude can request.
type Tool struct {
	Name        string
	Description string
	// Schema is the JSON schema of the input object: its "properties" and
	// "required" members (the top-level "type": "object" is implied).
	Schema        anthropic.ToolInputSchemaParam
	NeedsApproval bool
	// Propose builds the Proposal for an approval-gated tool. Required when
	// NeedsApproval is set; ignored otherwise.
	Propose func(input json.RawMessage) (Proposal, error)
	// Run executes the tool. For gated tools, d is the user's Decision (always
	// Approved when Run is called, with d.Proposal set); for others it is the
	// zero Decision. The returned string goes back to Claude as the tool
	// result.
	Run func(ctx context.Context, input json.RawMessage, d Decision) (string, error)
}

// Registry holds the tools offered to Claude.
type Registry struct {
	tools map[string]Tool
	order []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}} }

// Register adds t. It fails if the name is empty or already registered, Run
// is nil, Description is empty, or NeedsApproval is set without Propose.
func (r *Registry) Register(t Tool) error {
	switch {
	case t.Name == "":
		return errors.New("agent: tool name is empty")
	case t.Run == nil:
		return fmt.Errorf("agent: tool %q has no Run", t.Name)
	case t.Description == "":
		return fmt.Errorf("agent: tool %q has no description", t.Name)
	case t.NeedsApproval && t.Propose == nil:
		return fmt.Errorf("agent: tool %q needs approval but has no Propose", t.Name)
	}
	if _, dup := r.tools[t.Name]; dup {
		return fmt.Errorf("agent: tool %q already registered", t.Name)
	}
	r.tools[t.Name] = t
	r.order = append(r.order, t.Name)
	return nil
}

// Get returns the tool with the given name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Params returns the tool definitions for the Messages API, in registration
// order.
func (r *Registry) Params() []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		out = append(out, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: t.Schema,
		}})
	}
	return out
}

// Origin says how the user's message was entered.
type Origin int

const (
	Typed Origin = iota
	Voice        // dictated: may contain speech-recognition errors
)

// VoiceNote is prepended to dictated messages so Claude corrects misheard
// domain terms and confirms the corrected request before acting.
const VoiceNote = "[This message was dictated and may contain speech-recognition errors. " +
	"Correct misheard technical terms (cluster names, folder names, Slurm terms) " +
	"and state the corrected request back to the user before taking any action. " +
	"If it is still ambiguous, ask a clarifying question.]\n\n"

// EventKind classifies Events.
type EventKind int

const (
	EventText       EventKind = iota // a text delta from Claude (Text)
	EventToolCall                    // Claude requested a tool (Tool, Input)
	EventToolResult                  // a tool finished (Tool, Text = result, IsError)
	EventDeclined                    // the user declined a gated tool (Tool, Text = reason)
	// EventStep says what the turn is doing right now (Text, one short line
	// ending in "…", e.g. StepWaitingForClaude or "Connecting to bigcluster…").
	// Front ends show it in their busy indicator until the reply text starts
	// or the next step; it never asks anything and changes nothing.
	EventStep
	// EventApproved says the user approved a gated tool, which runs next
	// (Tool). Informational: front ends may ignore it.
	EventApproved
	// EventAutoApproved says the "Approve everything automatically" setting
	// approved a gated tool without asking (Tool, Text = the proposal's
	// Summary). EventApproved follows, as for a normal approval.
	EventAutoApproved
	// EventNote is a one-line notice about the conversation itself (Text,
	// e.g. NoteCompacted). Front ends show it as a muted system line.
	EventNote
)

// StepWaitingForClaude is the EventStep the loop emits before every model
// call (the broker round trip; the sign-in lookup may come first).
const StepWaitingForClaude = "Waiting for Claude…"

// StepThinking is the EventStep the loop emits when Claude starts a
// thinking block. The thinking text itself is never shown or logged.
const StepThinking = "Thinking…"

// StepSummarizing is the EventStep the loop emits before a compaction
// request (the earlier conversation is being summarized).
const StepSummarizing = "Summarizing earlier conversation…"

// NoteCompacted is the EventNote after the conversation was compacted.
const NoteCompacted = "Summarized earlier conversation"

// StepRunning is the busy-indicator text for a tool call ("Running
// cluster_info…"); front ends derive it from EventToolCall.
func StepRunning(tool string) string { return "Running " + tool + "…" }

// Event reports progress to the UI.
type Event struct {
	Kind    EventKind
	Tool    string
	Input   json.RawMessage
	Text    string
	IsError bool
}

// ErrMaxTurns used to be returned when Claude kept requesting tools past
// MaxTurns. Send no longer returns it: the turn ends with MaxTurnsMessage
// instead (M42.1). Kept so callers that test for it still compile.
var ErrMaxTurns = errors.New("agent: too many tool-use turns")

// MaxTurnsMessage is the assistant message that ends a turn which used all
// of its n model calls and still wanted tools.
func MaxTurnsMessage(n int) string {
	return fmt.Sprintf("I stopped after %d steps so you can check in. Say 'continue' and I'll go on.", n)
}

// ErrTruncated is returned when Claude's reply hit the token limit
// (stop reason max_tokens).
var ErrTruncated = errors.New("agent: reply truncated at the token limit")

// ErrRefused is returned when Claude declined to answer (stop reason refusal).
var ErrRefused = errors.New("agent: the model refused the request")

// ErrNoApprover is returned when a gated tool is requested but Agent.Approver
// is nil. Nothing runs.
var ErrNoApprover = errors.New("agent: no approver configured")

// ErrNoApproval may be returned (or wrapped) by a gated tool's Propose to say
// that this particular call needs no approval, for example a read-only
// command of a tool that is gated otherwise (remote_command). The loop then
// runs the tool like an ungated one, with the zero Decision, so Run must
// check again by itself that the input needs no approval.
var ErrNoApproval = errors.New("agent: this call needs no approval")

// Agent runs conversations.
type Agent struct {
	Model    Model
	Tools    *Registry
	Approver Approver
	System   string      // system prompt (include skill.Set.SystemPrompt())
	MaxTurns int         // model calls per Send; 0 means 25
	OnEvent  func(Event) // optional
	// SystemExtra, if set, is called once at the start of every Send; a
	// non-empty result is appended to System for every model call of that
	// Send. It carries per-turn context that the user can change between
	// turns (the chosen code and data folders), while System stays fixed
	// and prompt-cache friendly.
	SystemExtra func() string
	// CompactAt is the prompt size in tokens (input plus cache read and
	// cache write tokens of a reply) at which the conversation is
	// compacted before the next model turn; 0 means DefaultCompactAt, a
	// negative value never compacts.
	CompactAt int64
	// NoThinking turns off adaptive thinking on the planning request.
	NoThinking bool
	// OnReply, if set, is called with each complete model reply, including
	// compaction responses (stop reason compaction), e.g. to log token
	// usage. It must not change the message.
	OnReply func(*anthropic.BetaMessage)
}

// DefaultCompactAt is the default Agent.CompactAt: well inside the models'
// context windows and the broker's 2 MiB request cap, and the API's own
// default compaction trigger.
const DefaultCompactAt = 150_000

// Output token caps per request. The planning request allows more, since
// adaptive thinking counts against max_tokens; the broker lowers anything
// above its own cap (16384).
const (
	PlanMaxTokens    = 16000
	RoutineMaxTokens = 8192
)

// CompactionBeta is the Anthropic beta Labmill's compaction uses (the
// broker's allowlist forwards it to Bedrock as anthropic_beta).
const CompactionBeta = anthropic.AnthropicBetaCompact2026_09_04

// SkillTools returns the two read-only tools that expose a skill set to
// Claude (the "enum" is omitted when the set is empty, since the API rejects
// a null enum): load_skill {"name": string, enum of skill names} returning
// set.LoadSkillResult, and read_reference {"skill": string, "file": string}
// returning the reference content. Neither needs approval: they read only
// embedded or user-installed skill text, never user data.
func SkillTools(set *skill.Set) []Tool {
	var names []string
	for _, k := range set.List() {
		names = append(names, k.Name)
	}
	nameProp := func(desc string) map[string]any {
		p := map[string]any{"type": "string", "description": desc}
		if len(names) > 0 {
			p["enum"] = names
		}
		return p
	}
	return []Tool{
		{
			Name:        "load_skill",
			Description: "Load a skill's instructions. Call this when the user's request matches the skill's description.",
			Schema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"name": nameProp("The skill to load."),
				},
				Required: []string{"name"},
			},
			Run: func(_ context.Context, input json.RawMessage, _ Decision) (string, error) {
				var in struct {
					Name string `json:"name"`
				}
				if err := json.Unmarshal(input, &in); err != nil {
					return "", fmt.Errorf("bad input: %w", err)
				}
				return set.LoadSkillResult(in.Name)
			},
		},
		{
			Name:        "read_reference",
			Description: "Read one reference file of a skill, as named in the loaded skill's instructions.",
			Schema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"skill": nameProp("The skill that owns the file."),
					"file":  map[string]any{"type": "string", "description": "Reference file name as listed by load_skill, e.g. cli.md or bigiron/slurm.md (a path such as bigiron/references/slurm.md works too)."},
				},
				Required: []string{"skill", "file"},
			},
			Run: func(_ context.Context, input json.RawMessage, _ Decision) (string, error) {
				var in struct {
					Skill string `json:"skill"`
					File  string `json:"file"`
				}
				if err := json.Unmarshal(input, &in); err != nil {
					return "", fmt.Errorf("bad input: %w", err)
				}
				k, ok := set.Get(in.Skill)
				if !ok {
					return "", fmt.Errorf("unknown skill %q", in.Skill)
				}
				return k.ReadReference(in.File)
			},
		},
	}
}
