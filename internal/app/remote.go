package app

// remote_command (M28): one command on a cluster's login node, given as an
// argv list, never on the laptop.
//
// Mechanics, not restrictions (owner rule, M36.1: Labmill doesn't limit what
// runs on the remote HPC system; the cluster's own permissions decide):
//   - argv only. backend.Quote single-quotes every element, so the login
//     shell sees each one as a literal word: no pipes, redirections,
//     variables, globs or ~ expansion. NUL, control and format characters are
//     rejected (they could hide what an approval shows); count and length are
//     capped.
//   - Read-only commands (classifyRemote: Slurm queries, module, file
//     readers) run at once, on any path.
//   - Everything else shows the user the exact line for approval
//     (remoteNeedsApproval, the one place that decides). That is consent, not
//     a restriction.
//   - No terminal and no input (stdin closed): interactive programs simply
//     hang until the timeout. Every command runs under coreutils `timeout`
//     (default 60 s, max 10 min); output keeps the last 64 KB (stderr 16 KB),
//     with control characters removed, and the exit code is reported.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/textsafe"
)

const (
	remoteMaxArgs     = 64       // argv elements
	remoteMaxArgBytes = 4096     // bytes per element
	remoteMaxBytes    = 16 << 10 // bytes in all
	remoteDefTimeout  = 60 * time.Second
	remoteMaxTimeout  = 10 * time.Minute
	remoteKillAfter   = "5"      // seconds `timeout` waits after TERM before KILL
	remoteMaxScript   = 64 << 10 // bytes in a script
	remoteMaxStdout   = 64 << 10
	remoteMaxStderr   = 16 << 10
	// remoteMaxSummary caps the command shown in an approval's one-line
	// summary (the details show all of it).
	remoteMaxSummary = 160
)

// remoteSlack is how much longer than the remote timeout Labmill waits before
// it gives up on the connection itself (a variable for tests).
var remoteSlack = 20 * time.Second

// remoteChangeNote is the approval's warning for a command that isn't on the
// read-only list.
const remoteChangeNote = "This command can change files on the cluster. Approve it only if you want it to run."

// remoteModuleScript runs `module` (a shell function from Lmod or
// Environment Modules, so it needs a login shell) with the user's words as
// quoted positional parameters; module writes to stderr, so it is merged.
const remoteModuleScript = `module "$@" 2>&1`

// remoteMlScript is the same for `ml`, Lmod's shortcut (ml X loads X).
const remoteMlScript = `ml "$@" 2>&1`

// remoteNeedsApproval is the one place that decides whether a command waits
// for the user's OK. Default: only the read-only list runs at once. With the
// user's setting "Run commands on the cluster without asking" (auto, from
// Env.AutoRunRemote) every other command runs at once too, except a command
// that prints a file named as a sign-in secret (rc.secret), which still asks
// because its content goes to Claude.
func remoteNeedsApproval(rc remoteCmd, auto bool) bool {
	if rc.secret != "" {
		return true
	}
	if auto {
		return false
	}
	return rc.class != remoteReadOnly
}

// autoRunRemote reports the user's setting "Run commands on the cluster
// without asking"; off when the app has no such setting. It is read for
// every command, so a change applies at once.
func (t *toolset) autoRunRemote() bool {
	return t.env.AutoRunRemote != nil && t.env.AutoRunRemote()
}

// remoteAutoNote starts the result of a command that was not on the
// read-only list and ran because of the user's setting.
const remoteAutoNote = agent.AutoRanPrefix + ", per the user's setting)"

// remoteSecretNote is added to an approval when a read-only command names a
// file that holds sign-in secrets: the output goes to Claude's context.
const remoteSecretNote = "This file's content will be sent to Claude. It looks like it holds sign-in secrets (keys, passwords or tokens)."

// remoteClass is how a command may run.
type remoteClass int

const (
	remoteReadOnly remoteClass = iota // runs at once
	remoteGated                       // needs the user's approval
)

// remoteCmd is a validated remote_command request.
type remoteCmd struct {
	cluster config.Cluster
	argv    []string // as Claude gave it
	class   remoteClass
	why     string   // remoteGated: why it needs approval
	wire    []string // what runs on the login node (under timeout)
	timeout time.Duration
	// sideEffects: a command that is not on the read-only list and also names a sign-in secret.
	sideEffects bool
	secret      string // read-only command that names a sign-in secret (secretArg); it asks first
	// autoRan: a gated command that ran without approval because of the
	// user's setting; its output is redacted like file content, since no
	// person looked at the command first.
	autoRan bool
	// script is multi-line shell code for the interpreter in argv (default
	// bash -s). It travels on stdin, never in argv, so the login shell
	// (tcsh included) and newlines don't matter. Always remoteGated.
	script string
}

// display is the command as the user and Claude see it.
func (rc remoteCmd) display() string {
	if rc.script != "" {
		return backend.Quote(rc.argv) + " (script on stdin)"
	}
	return backend.Quote(rc.argv)
}

// remoteSubmitHint starts the result of a command that submits a job
// through remote_command. Nothing is refused; submit_job is simply better.
const remoteSubmitHint = "(Use submit_job for submissions: it records the job, shows the review and monitors it.)"

// remoteDefaultInterpreter reads a script from stdin.
var remoteDefaultInterpreter = []string{"bash", "-s"}

// remoteInput is remote_command's input.
type remoteInput struct {
	Cluster        string   `json:"cluster"`
	Argv           []string `json:"argv"`
	Script         string   `json:"script"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

func (t *toolset) remoteCommand() agent.Tool {
	return agent.Tool{
		Name: "remote_command",
		Description: "Run one command on a cluster's login node (never on this computer) and get its output and exit code. " +
			"Give the command as argv, a list of words; each word is passed literally: there is no shell, so no pipes, redirections, ';', '&&', variables or wildcards, and '~' only works at the start of a file path of the file commands below. " +
			"Labmill puts no limits on what you may run there: the cluster's own permissions decide what the user's account can do. " +
			"Read-only commands run at once, on any path the account can read: squeue and sinfo (not --iterate), sacct, sshare, sprio, sstat, seff, scontrol show job|node|partition|reservation|config, sacctmgr show|list, module [-r|-t…] avail|spider|list|show|whatis|keyword (module and ml always run through a login shell, since they are shell functions), command -v|-V <name>, and ls, cat, head, tail (not -f), wc, du, df, findmnt (not --poll), lsblk, mount (no arguments), stat and file. " +
			"Every other command is shown to the user for approval first, with the exact line that runs, so say what it does and why before calling it, unless the user turned on 'Run commands on the cluster without asking' or 'Approve everything automatically': then it runs at once, so be as careful as if no one will check it (a command that names a sign-in secret file still asks; output of an unreviewed command is redacted). " +
			"A read that prints a file whose name marks it as holding sign-in secrets (cat, head, tail; by name only, so symlinks, hard links, /proc/*/environ and tokens inside ordinary files aren't detected; SSH private keys, ~/.config/tool/authentication.json, .netrc, cloud credentials, tokens, *.pem, *.key, shell histories) also asks first, and the approval warns that its content goes to Claude; ~/.ssh/config and .bashrc are read at once, and lines that assign a password, token or key are redacted in what comes back from cat, head and tail. " +
			"For multi-line shell code (loops, here-documents, several steps) give `script` instead of putting it in a word: it goes to the interpreter's stdin (default `bash -s`, which is not a login shell, so `module` is undefined: use [\"bash\", \"-l\", \"-s\"] when the script needs modules; argv may name another interpreter, e.g. [\"sh\", \"-s\"]), so quoting and newlines are safe; at most 64 KB. A script is always shown to the user in full for approval (unless they turned on 'Run commands on the cluster without asking'). " +
			"Job scripts and helper files come from draft_job (its `files`), never from a script written with this tool. Submit only with submit_job, never sbatch or srun here (such a command still runs after approval, but its result says to use submit_job: submit_job records the job, shows the review and monitors it). Check jobs with job_status and job_logs, and bring results back with fetch_results. " +
			"Prefer the specific tools when one fits (cluster_info, cluster_jobs, job_status, job_logs, draft_job/submit_job, cancel_job). " +
			"It runs without a terminal and without input, so interactive programs won't work (they hang until the timeout). " +
			"Output keeps the last 64 KB. timeout_seconds: default 60, at most 600.",
		Schema: objSchema(map[string]any{
			"cluster":         strProp("cluster name from list_clusters; the command runs on its login node"),
			"argv":            strList("the command and its arguments, one word per element, e.g. [\"squeue\", \"--me\"]; with `script`, optional: the interpreter (default [\"bash\", \"-s\"])"),
			"script":          strProp("optional multi-line shell code, sent on stdin to the interpreter in argv (default bash -s); up to 64 KB; always shown for approval"),
			"timeout_seconds": intProp("stop the command after this many seconds (default 60, max 600)"),
		}, "cluster"),
		NeedsApproval: true,
		Propose: func(input json.RawMessage) (agent.Proposal, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			rc, err := t.remotePlan(ctx, input)
			if err != nil {
				return agent.Proposal{}, err
			}
			if !remoteNeedsApproval(rc, t.autoRunRemote()) {
				return agent.Proposal{}, agent.ErrNoApproval
			}
			return t.remoteProposal(rc), nil
		},
		Run: func(ctx context.Context, input json.RawMessage, d agent.Decision) (string, error) {
			rc, err := t.remotePlan(ctx, input)
			if err != nil {
				return "", err
			}
			note := ""
			switch {
			case !remoteNeedsApproval(rc, t.autoRunRemote()):
				// Read-only, or the user's setting lets everything run at once.
				if rc.class == remoteGated && (!d.Approved || d.Auto) {
					note = remoteAutoNote + "\n"
					rc.autoRan = true
				}
			case !d.Approved:
				return "", errors.New("This command needs the user's approval, which it didn't get.")
			default:
				if err := sameAsApproved(t.remoteProposal(rc), d.Proposal); err != nil {
					return "", errors.New("This command changed since the user approved it. Please ask again.")
				}
				if d.Auto {
					// "Approve everything automatically": nobody looked, so the
					// same safeguards as an auto-run apply.
					note = remoteAutoNote + "\n"
					rc.autoRan = true
				}
			}
			out, err := t.runRemote(ctx, rc)
			if err == nil && submitsJob(rc) {
				// After the auto-run note, so the runner sees it first and
				// adds no second note.
				note += remoteSubmitHint + "\n"
			}
			return note + out, err
		},
	}
}

// remotePlan decodes and validates the input and classifies the command.
func (t *toolset) remotePlan(ctx context.Context, input json.RawMessage) (remoteCmd, error) {
	var in remoteInput
	if err := decode(input, &in); err != nil {
		return remoteCmd{}, err
	}
	c, err := t.cluster(in.Cluster)
	if err != nil {
		return remoteCmd{}, err
	}
	if c.SSHHost == "" {
		return remoteCmd{}, fmt.Errorf("Cluster %s has no login host configured, so Labmill can't run commands there.", c.Name)
	}
	timeout := remoteDefTimeout
	switch {
	case in.TimeoutSeconds < 0 || time.Duration(in.TimeoutSeconds)*time.Second > remoteMaxTimeout:
		return remoteCmd{}, fmt.Errorf("timeout_seconds must be between 1 and %d.", int(remoteMaxTimeout/time.Second))
	case in.TimeoutSeconds > 0:
		timeout = time.Duration(in.TimeoutSeconds) * time.Second
	}
	if in.Script != "" {
		return t.scriptPlan(c, in, timeout)
	}
	if err := validateArgv(in.Argv); err != nil {
		return remoteCmd{}, err
	}
	class, why, inner := classifyRemote(in.Argv)
	// Not a restriction: an approved command may read anything the account
	// can. But a read-only command must not pipe keys and tokens into the
	// model's context on its own, so it asks first.
	secret, sideEffects := "", false
	// Any other command (bash -c "cat …", base64, grep …) could print such a
	// file as well, so it is checked by name too; that matters when the
	// user's setting lets other commands run without asking.
	switch {
	case class == remoteReadOnly && printsContent(in.Argv):
		if tok, ok := secretArg(in.Argv); ok {
			class, secret, why = remoteGated, tok, "the file looks like it holds sign-in secrets, and its content would be sent to Claude"
		}
	case class == remoteGated:
		if tok, ok := secretArg(in.Argv); ok {
			secret, sideEffects = tok, true
			why += "; it also names a file that looks like it holds sign-in secrets, and its output would be sent to Claude"
		}
	}
	return remoteCmd{
		cluster: c, argv: in.Argv, class: class, why: why,
		wire:    timeoutWire(timeout, inner),
		timeout: timeout,
		secret:  secret, sideEffects: sideEffects,
	}, nil
}

// scriptPlan validates a remote_command with a script. It is always gated:
// nothing about shell code can be judged read-only. The secret-file name
// check runs over the script's words, as for any other gated command.
func (t *toolset) scriptPlan(c config.Cluster, in remoteInput, timeout time.Duration) (remoteCmd, error) {
	if err := validateScript(in.Script); err != nil {
		return remoteCmd{}, err
	}
	argv := in.Argv
	if len(argv) == 0 {
		argv = remoteDefaultInterpreter
	}
	if err := validateArgv(argv); err != nil {
		return remoteCmd{}, err
	}
	rc := remoteCmd{
		cluster: c, argv: argv, class: remoteGated, script: in.Script,
		why:     "it is a script, and Labmill can't tell in advance what a script does",
		wire:    timeoutWire(timeout, argv),
		timeout: timeout,
	}
	if tok, ok := secretArg(append(append([]string(nil), argv...), strings.Fields(in.Script)...)); ok {
		rc.secret, rc.sideEffects = tok, true
		rc.why += "; it also names a file that looks like it holds sign-in secrets, and its output would be sent to Claude"
	}
	return rc, nil
}

// validateScript checks a script: valid UTF-8, no NUL, at most
// remoteMaxScript bytes. Newlines, tabs and other text are fine (the script
// goes to stdin, never into a command line).
func validateScript(s string) error {
	switch {
	case len(s) > remoteMaxScript:
		return fmt.Errorf("The script is longer than %d KB.", remoteMaxScript>>10)
	case !utf8.ValidString(s):
		return errors.New("The script is not valid UTF-8 text.")
	case strings.ContainsRune(s, 0):
		return errors.New("The script contains a NUL character, which isn't allowed.")
	}
	return nil
}

// submitsJob reports whether rc submits a job: sbatch or srun at the start of a command (argv, a `-c` string in argv, or a line
// or ;/&&/||/| part of the script). A hint only; nothing is refused.
func submitsJob(rc remoteCmd) bool {
	texts := []string{rc.script}
	if rc.script == "" {
		if submitCommand(rc.argv) {
			return true
		}
		for i := 1; i < len(rc.argv); i++ {
			if p := rc.argv[i-1]; p == "-c" || p == "-lc" || p == "-ic" {
				texts = append(texts, rc.argv[i])
			}
		}
	}
	sep := strings.NewReplacer("&&", "\n", "||", "\n", ";", "\n", "|", "\n", "(", "\n", "{", "\n", "`", "\n")
	for _, text := range texts {
		for _, seg := range strings.Split(sep.Replace(text), "\n") {
			if submitCommand(strings.Fields(seg)) {
				return true
			}
		}
	}
	return false
}

// submitCommand reports whether words (after leading NAME=value, exec,
// time, nohup and similar) start a job submission.
func submitCommand(words []string) bool {
	for len(words) > 0 {
		w := words[0]
		if strings.Contains(w, "=") && !strings.HasPrefix(w, "-") || w == "exec" || w == "time" || w == "nohup" || w == "if" || w == "!" || w == "env" || w == "command" || w == "then" || w == "do" || w == "else" {
			words = words[1:]
			continue
		}
		break
	}
	if len(words) == 0 {
		return false
	}
	switch path.Base(words[0]) {
	case "sbatch", "srun":
		return true
	}
	return false
}

// timeoutWire wraps inner in coreutils timeout, so the command also stops
// on the cluster (closing the SSH session alone would leave it running).
func timeoutWire(d time.Duration, inner []string) []string {
	return append([]string{"timeout", "-k", remoteKillAfter, strconv.Itoa(int(d / time.Second))}, inner...)
}

// gatedInner is what an approved command runs: argv itself, except that
// module and ml (shell functions from the login profile) run through a
// login shell.
func gatedInner(argv []string) []string {
	switch argv[0] {
	case "module":
		return append([]string{"bash", "-lc", remoteModuleScript, "labmill-module"}, argv[1:]...)
	case "ml":
		return append([]string{"bash", "-lc", remoteMlScript, "labmill-ml"}, argv[1:]...)
	}
	return argv
}

// remoteProposal is the approval of a gated command.
func (t *toolset) remoteProposal(rc remoteCmd) agent.Proposal {
	disp := rc.display()
	short := disp
	if utf8.RuneCountInString(short) > remoteMaxSummary {
		short = string([]rune(short)[:remoteMaxSummary]) + "…"
	}
	if rc.script != "" {
		short = "a script via " + backend.Quote(rc.argv)
		if utf8.RuneCountInString(short) > remoteMaxSummary {
			short = string([]rune(short)[:remoteMaxSummary]) + "…"
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Command: %s\n", disp)
	fmt.Fprintf(&b, "Where: the login node of %s, %s\n", rc.cluster.Name, t.loginVia(rc.cluster))
	fmt.Fprintf(&b, "Runs exactly as: %s\n", backend.Quote(rc.wire))
	fmt.Fprintf(&b, "It is stopped after %d seconds. It runs without a terminal and without input, so it can't ask questions.\n\n", int(rc.timeout/time.Second))
	if rc.script != "" {
		fmt.Fprintf(&b, "The script (sent on stdin; %d bytes), in full:\n%s\n", len(rc.script), approvalScript(rc.script))
		b.WriteString("--- end of script ---\n\n")
	}
	if rc.secret != "" {
		b.WriteString(remoteSecretNote + "\n")
	}
	if rc.secret == "" || rc.sideEffects {
		b.WriteString(remoteChangeNote + "\n")
	}
	fmt.Fprintf(&b, "Why Labmill asks: %s.", rc.why)
	return agent.Proposal{
		Tool:    "remote_command",
		Summary: fmt.Sprintf("Run on %s: %s", sanitize(rc.cluster.Name), short),
		Details: b.String(),
		Secret:  rc.secret != "",
	}
}

// approvalScript renders a script for the approval: newlines and tabs stay,
// other control and format characters are replaced so they can't alter the
// display. Invalid text was refused earlier.
func approvalScript(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return '\uFFFD'
		}
		return r
	}, s)
}

// loginVia is the login host as an approval shows it: the SSH alias plus the
// real HostName when Env.HostName knows it.
func (t *toolset) loginVia(c config.Cluster) string {
	alias := c.SSHHost
	if t.env.HostName != nil {
		if h := t.env.HostName(alias); h != "" && h != alias {
			return sanitize(alias + " (" + h + ")")
		}
	}
	return sanitize(alias)
}

// runRemote runs rc on the cluster's login node and formats the result.
func (t *toolset) runRemote(ctx context.Context, rc remoteCmd) (string, error) {
	if t.env.LoginRunner == nil {
		return "", errors.New("Running commands on the cluster isn't available right now.")
	}
	r, err := t.env.LoginRunner(ctx, rc.cluster)
	if err != nil {
		return "", fmt.Errorf("I couldn't connect to %s: %w", rc.cluster.Name, err)
	}
	cctx, cancel := context.WithTimeout(ctx, rc.timeout+remoteSlack)
	defer cancel()
	var stdin io.Reader
	if rc.script != "" {
		stdin = strings.NewReader(rc.script)
	}
	res, err := r.Run(cctx, rc.wire, stdin)
	secs := int(rc.timeout / time.Second)
	if err != nil {
		if ctx.Err() == nil && errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("%s on %s didn't finish within %d seconds and was stopped.", rc.display(), rc.cluster.Name, secs)
		}
		return "", fmt.Errorf("I couldn't run %s on %s: %w", rc.display(), rc.cluster.Name, err)
	}
	// A laptop-side rule on what reaches Claude, as for preview_file: lines
	// that assign a password, token or key are hidden in file content.
	if printsContent(rc.argv) || rc.secret != "" || rc.autoRan {
		res.Stdout = redactSecrets(res.Stdout)
	}
	return formatRemote(rc, res), nil
}

// formatRemote renders a finished command for Claude. The output and errors
// are cleaned with textsafe.Clean (escape sequences, bidi and other format
// characters removed whole), then the whole result with cleanText, like
// job_logs.
func formatRemote(rc remoteCmd, res backend.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Ran on %s (%s): %s\n", rc.cluster.Name, sanitize(rc.cluster.SSHHost), rc.display())
	fmt.Fprintf(&b, "Exit code: %d", res.ExitCode)
	switch res.ExitCode {
	case 0:
	case 124:
		fmt.Fprintf(&b, " (stopped after %d seconds: it didn't finish in time)", int(rc.timeout/time.Second))
	case 127:
		b.WriteString(" (the command was not found on the login node)")
	}
	b.WriteString("\n")
	if out, head := capOutput(res.Stdout, remoteMaxStdout, "Output"); out != "" {
		b.WriteString(head + "\n" + out + "\n")
	} else {
		b.WriteString("Output: (none)\n")
	}
	if out, head := capOutput(res.Stderr, remoteMaxStderr, "Errors"); out != "" {
		b.WriteString(head + "\n" + out + "\n")
	}
	// The same final cleaning as the other tools that relay cluster output
	// (job_logs): no control characters survive, header lines included.
	return cleanText(strings.TrimRight(b.String(), "\n"))
}

// capOutput cleans one output stream and keeps at most max bytes of it, with
// its heading ("<label>:", or a note on what was kept). Normally the end is
// kept (the last lines matter most). When SSHRunner already cut the stream
// at 4 MB (backend.TruncatedMarker), its end is gone, so the start is kept
// and the heading says so.
func capOutput(s string, max int, label string) (out, head string) {
	if first, ok := strings.CutSuffix(s, backend.TruncatedMarker); ok {
		out, _ = headCap(first, max)
		return strings.TrimRight(textsafe.Clean(out), "\n"),
			fmt.Sprintf("%s (cut at 4 MB on the cluster; only the first %d KB are shown):", label, max>>10)
	}
	out, cut := tailCap(s, max)
	out = strings.TrimRight(textsafe.Clean(out), "\n")
	if cut {
		return out, fmt.Sprintf("%s (only the last %d KB):", label, max>>10)
	}
	return out, label + ":"
}

// headCap keeps at most max bytes from the start of s, ending at a line
// boundary when there is one in the kept part (and always at a rune start).
func headCap(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	s = s[:max]
	if i := strings.LastIndexByte(s, '\n'); i > 0 {
		return s[:i+1], true
	}
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s, true
}

// tailCap keeps at most max bytes from the end of s, starting at a line
// boundary when there is one in the kept part (and always at a rune start).
// cut reports whether anything was dropped.
func tailCap(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	s = s[len(s)-max:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
		return s[i+1:], true
	}
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s, true
}

// ---- validation and classification ---------------------------------------

// printsContent reports whether a read-only command prints file content
// (cat, head, tail, file -z); ls, stat, du and wc don't, so they auto-run
// even on a key path.
func printsContent(argv []string) bool {
	switch argv[0] {
	case "cat", "head", "tail":
		return true
	case "file":
		for _, a := range argv[1:] {
			if a == "-z" || a == "--uncompress" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "z")) {
				return true
			}
		}
	}
	return false
}

// secretArg looks for a file that holds sign-in secrets in a read-only
// command (the match is by name only: symlinks, hard links, /proc/*/environ and
// tokens inside ordinary files such as .bashrc aren't detected): each word as a whole and every path-like piece of it. It decides
// only whether the command asks first (never refusal), so it is minimal:
// privacy.go's hardSecretPath (SSH private keys, ~/.config/tool/authentication.json,
// .netrc, cloud and Hugging Face tokens, *.pem, *.key, histories ...), except
// public material (*.pub, ~/.ssh/config, config.d/*, known_hosts and
// authorized_keys: host names and public keys, not secrets). Returns the offending piece.
func secretArg(argv []string) (string, bool) {
	for _, a := range argv {
		for _, p := range append([]string{a}, pathPieces(a)...) {
			if hardSecretPath(p) && !publicSecretLookalike(p) {
				return sanitize(p), true
			}
		}
	}
	return "", false
}

// publicSecretLookalike reports whether p, inside a secrets folder, is public
// material.
func publicSecretLookalike(p string) bool {
	base := path.Base(p)
	if strings.HasSuffix(base, ".pub") {
		return true
	}
	dir := path.Base(path.Dir(p))
	if dir == ".ssh" && (base == "config" || base == "known_hosts" || base == "authorized_keys") {
		return true
	}
	return strings.Contains(p, ".ssh/config.d/")
}

// pathPieces splits a at characters that can't be part of a plain path.
func pathPieces(a string) []string {
	return strings.FieldsFunc(a, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._-/~+@%", r))
	})
}

// validateArgv checks the shape of argv: 1 to remoteMaxArgs elements, the
// first not empty, each valid UTF-8 without control or format characters,
// at most remoteMaxArgBytes each and remoteMaxBytes in all.
func validateArgv(argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return errors.New("argv must name a command, for example [\"squeue\", \"--me\"].")
	}
	if len(argv) > remoteMaxArgs {
		return fmt.Errorf("That command has %d words; at most %d are allowed.", len(argv), remoteMaxArgs)
	}
	total := 0
	for i, a := range argv {
		if len(a) > remoteMaxArgBytes {
			return fmt.Errorf("Word %d of the command is longer than %d bytes.", i+1, remoteMaxArgBytes)
		}
		total += len(a)
		if err := validateArg(a); err != nil {
			return fmt.Errorf("Word %d of the command %v.", i+1, err)
		}
	}
	if total > remoteMaxBytes {
		return fmt.Errorf("The command is longer than %d bytes.", remoteMaxBytes)
	}
	return nil
}

// validateArg refuses invalid UTF-8 and control or format characters (NUL,
// newline, tab, escape, bidi controls, ...), which could hide what an
// approval shows.
func validateArg(a string) error {
	if !utf8.ValidString(a) {
		return errors.New("is not valid text")
	}
	for _, r := range a {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' {
			return errors.New("contains a control character (such as a newline or tab), which isn't allowed in argv; put multi-line shell code in `script` instead")
		}
	}
	return nil
}

// leadingWords returns the leading arguments that aren't flags (up to the
// first one starting with '-') and the rest.
func leadingWords(args []string) (words, rest []string) {
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			return args[:i], args[i:]
		}
	}
	return args, nil
}

// notListed is the reason for a command that isn't on the read-only list.
const notListed = "it is not on Labmill's list of read-only commands"

// classifyRemote decides whether argv is on the read-only list (and so runs
// without approval) and returns what runs inside timeout.
//
// Read-only (no approval):
//   - squeue, sinfo: any flags but --iterate/-i (they repeat forever);
//   - sacct: any flags but -f/--file (another accounting file);
//   - sshare, sprio, sstat, seff: any flags;
//   - scontrol [-o|--oneliner|-d|--details|-a|--all]... show
//     job|jobs|node|nodes|partition|partitions|reservation|reservations|config
//     [one name or id];
//   - sacctmgr [flags without -i/--immediate] show|list ...;
//   - module [-t|--terse|-r|--regexp|-d|--default|--show_hidden|-q|--quiet|
//     --name=value] avail|av|spider|list|show|display|whatis|keyword|key
//     [module names: words not starting with '-', '/' or '~' and without
//     ".." (a path would make `module show` read any modulefile; avail,
//     spider and keyword also take patterns such as '.*torch.*', but not
//     "./")], via `bash -lc 'module "$@" 2>&1'`; every module and ml call,
//     approved ones included, runs that way (gatedInner);
//   - findmnt (not --poll/-p, --tab-file/-F), lsblk and mount without
//     arguments;
//   - command -v|-V <program names> (names not starting with '-'), via
//     `bash -c 'command "$@"'` with the flag, "--" and the names as
//     positional parameters (command is a shell builtin; plain command runs
//     the program and needs approval);
//   - mount without arguments;
//   - ls, cat, head, tail, wc, du, df, findmnt, lsblk, stat, file: flags except those that
//     follow files forever or read file names from a file (tail -f/-F/
//     --follow/--pid/--retry, du -X/--exclude-from/--files0-from, wc
//     --files0-from, file -f/-m/-C/--files-from/--magic-file/--compile; GNU
//     abbreviations of those long options count too, but an exact option
//     name wins, so du --exclude=PAT is fine; a long option that takes a
//     value may have it in the next word only when written in full), on any
//     path the account can read (relative paths are relative to the home
//     folder; a leading ~/ is mapped to ./, since argv has no shell).
//
// Everything else is remoteGated, with the reason.
func classifyRemote(argv []string) (remoteClass, string, []string) {
	name, args := argv[0], argv[1:]
	gated := func(why string) (remoteClass, string, []string) { return remoteGated, why, gatedInner(argv) }
	switch name {
	case "squeue", "sinfo":
		for _, a := range args {
			if strings.HasPrefix(a, "--") {
				if f, _, _ := strings.Cut(a, "="); len(f) > 2 && strings.HasPrefix("--iterate", f) {
					return gated(name + " --iterate repeats forever")
				}
				continue
			}
			if strings.HasPrefix(a, "-") && strings.ContainsRune(a, 'i') {
				return gated(name + " -i repeats forever")
			}
		}
		return remoteReadOnly, "", argv
	case "sacct":
		for _, a := range args {
			if f, _, _ := strings.Cut(a, "="); strings.HasPrefix(a, "--") && len(f) > 2 && strings.HasPrefix("--file", f) {
				return gated("sacct --file reads another accounting file")
			}
			if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a, 'f') {
				return gated("sacct -f reads another accounting file")
			}
		}
		return remoteReadOnly, "", argv
	case "sshare", "sprio", "sstat", "seff":
		return remoteReadOnly, "", argv
	case "scontrol":
		if scontrolShow(args) {
			return remoteReadOnly, "", argv
		}
		return gated("only scontrol show job, node, partition, reservation or config is read-only")
	case "sacctmgr":
		words, _ := leadingWords(skipFlags(args))
		for _, a := range args {
			if a == "--immediate" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a, 'i')) {
				return gated("sacctmgr -i makes changes without asking")
			}
		}
		if len(words) > 0 && (words[0] == "show" || words[0] == "list") {
			return remoteReadOnly, "", argv
		}
		return gated("only sacctmgr show or list is read-only")
	case "module":
		if moduleReadOnly(args) {
			return remoteReadOnly, "", gatedInner(argv)
		}
		return gated("only module avail, spider, list, show, whatis or keyword with plain module names is read-only")
	case "mount":
		if len(args) == 0 {
			return remoteReadOnly, "", argv
		}
		return gated("only mount without arguments (the list of mounts) is read-only")
	case "command":
		if commandVReadOnly(args) {
			return remoteReadOnly, "", append([]string{"bash", "-c", `command "$@"`, "labmill-command", args[0], "--"}, args[1:]...)
		}
		return gated("only command -v or -V with plain program names is read-only (plain command runs the program)")
	}
	if spec, ok := readCmds[name]; ok {
		mapped, why := fileReadArgs(spec, args)
		if why != "" {
			return gated(why)
		}
		return remoteReadOnly, "", append([]string{name}, mapped...)
	}
	return gated(notListed)
}

// skipFlags drops leading flags (arguments starting with '-').
func skipFlags(args []string) []string {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			return args[i:]
		}
	}
	return nil
}

var scontrolEntities = map[string]bool{
	"job": true, "jobs": true, "node": true, "nodes": true, "partition": true, "partitions": true,
	"reservation": true, "reservations": true, "config": true,
}

// scontrolShow reports whether args are a read-only `scontrol show`.
func scontrolShow(args []string) bool {
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		switch args[i] {
		case "-o", "--oneliner", "-d", "--details", "-a", "--all", "-od", "-do":
		default:
			return false
		}
	}
	rest := args[i:]
	if len(rest) < 2 || len(rest) > 3 || rest[0] != "show" || !scontrolEntities[rest[1]] {
		return false
	}
	return len(rest) == 2 || !strings.HasPrefix(rest[2], "-")
}

var moduleRO = map[string]bool{
	"avail": true, "av": true, "spider": true, "list": true, "show": true, "display": true,
	"whatis": true, "keyword": true, "key": true,
}

// moduleFlags are the leading module options known to take no value.
var moduleFlags = map[string]bool{
	"-t": true, "--terse": true, "-r": true, "--regexp": true, "-d": true, "--default": true,
	"--show_hidden": true, "--show-hidden": true, "-q": true, "--quiet": true,
}

// moduleReadOnly reports whether args are a read-only module subcommand.
func moduleReadOnly(args []string) bool {
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		// Only known flags without a value: an unknown option might take
		// the next word (module -w avail save), which is then the command.
		if _, _, eq := strings.Cut(args[i], "="); !moduleFlags[args[i]] && !(eq && strings.HasPrefix(args[i], "--")) {
			return false
		}
	}
	if i >= len(args) || !moduleRO[args[i]] {
		return false
	}
	// avail, spider and keyword take patterns (module -r spider '.*torch.*'),
	// so a leading '.' is fine there; show, display and whatis read a
	// modulefile named by a path, so it isn't.
	pattern := false
	switch args[i] {
	case "avail", "av", "spider", "keyword", "key":
		pattern = true
	}
	for _, a := range args[i+1:] {
		if strings.HasPrefix(a, "-") || strings.HasPrefix(a, "/") || strings.HasPrefix(a, "~") || strings.Contains(a, "..") {
			return false
		}
		if strings.HasPrefix(a, ".") && (!pattern || strings.HasPrefix(a, "./")) {
			return false
		}
	}
	return true
}

// commandVReadOnly reports whether args are exactly -v or -V followed by one
// or more names not starting with '-' (lookup only; `command foo` would run foo).
func commandVReadOnly(args []string) bool {
	if len(args) < 2 || (args[0] != "-v" && args[0] != "-V") {
		return false
	}
	for _, a := range args[1:] {
		if a == "" || strings.HasPrefix(a, "-") {
			return false
		}
	}
	return true
}

// readSpec is how a read-only file command's flags are parsed.
type readSpec struct {
	valueFlags string   // short flags whose value may be the next argument
	valueLong  []string // long options that take a value (--opt=V or --opt V)
	denyShort  string   // short flags that need approval
	denyLong   []string // long options that need approval (abbreviations too)
}

// readCmds are the read-only file commands (GNU coreutils and file).
var readCmds = map[string]readSpec{
	"ls": {valueFlags: "ITw", valueLong: []string{"--block-size", "--format", "--hide", "--ignore", "--indicator-style",
		"--quoting-style", "--sort", "--tabsize", "--time", "--time-style", "--width"}},
	"cat":  {},
	"head": {valueFlags: "nc", valueLong: []string{"--lines", "--bytes"}},
	"tail": {valueFlags: "ncs", valueLong: []string{"--lines", "--bytes", "--sleep-interval", "--max-unchanged-stats"},
		denyShort: "fF", denyLong: []string{"--follow", "--pid", "--retry"}},
	"wc": {denyLong: []string{"--files0-from"}},
	"du": {valueFlags: "dBt", valueLong: []string{"--block-size", "--max-depth", "--threshold", "--exclude", "--time-style"},
		denyShort: "X", denyLong: []string{"--files0-from", "--exclude-from"}},
	"df":   {valueFlags: "Btx", valueLong: []string{"--block-size", "--type", "--exclude-type"}},
	"stat": {valueFlags: "c", valueLong: []string{"--format", "--printf"}},
	// findmnt lists mounted file systems; --poll waits forever and
	// --tab-file reads another mount table.
	"findmnt": {valueFlags: "oOStTNMQ", valueLong: []string{"--output", "--options", "--source", "--types", "--target", "--task", "--mountpoint", "--filter", "--submounts-of"},
		denyShort: "pF", denyLong: []string{"--poll", "--tab-file"}},
	"lsblk": {valueFlags: "oEIJmxeOtT", valueLong: []string{"--output", "--sort", "--exclude", "--include", "--filter"}},
	"file": {valueFlags: "FeP", valueLong: []string{"--separator", "--exclude", "--exclude-quiet", "--parameter"},
		denyShort: "fmC", denyLong: []string{"--files-from", "--magic-file", "--compile"}},
}

// longOption classifies a long option of a file command: ok, whether its
// value is the next argument, or why it needs approval. An exact name wins
// (getopt's rule); an abbreviation of a denied option needs approval, and so
// does an abbreviation of a value option with the value in the next word
// (Labmill can't be sure which option getopt picks, and a path taken for a
// value would escape the path checks).
func (spec readSpec) longOption(a string) (takesNext bool, why string) {
	f, _, hasEq := strings.Cut(a, "=")
	for _, d := range spec.denyLong {
		if f == d {
			return false, "the option " + f + " needs approval"
		}
	}
	for _, v := range spec.valueLong {
		if f == v {
			return !hasEq, ""
		}
	}
	for _, d := range spec.denyLong {
		if strings.HasPrefix(d, f) {
			return false, "the option " + f + " needs approval"
		}
	}
	if !hasEq {
		for _, v := range spec.valueLong {
			if strings.HasPrefix(v, f) {
				return false, "the abbreviated option " + f + " needs approval (write it in full, or as " + v + "=VALUE)"
			}
		}
	}
	return false, ""
}

// fileReadArgs parses a file command's arguments: it returns them with the
// paths mapped (~ → ., ~/x → ./x), or why the command needs approval.
func fileReadArgs(spec readSpec, args []string) (mapped []string, why string) {
	endOpts := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case !endOpts && a == "--":
			endOpts = true
			mapped = append(mapped, a)
		case !endOpts && strings.HasPrefix(a, "--"):
			next, why := spec.longOption(a)
			if why != "" {
				return nil, why
			}
			mapped = append(mapped, a)
			if next && i+1 < len(args) {
				i++
				mapped = append(mapped, args[i])
			}
		case !endOpts && strings.HasPrefix(a, "-") && len(a) > 1:
			group := a[1:]
			if strings.ContainsAny(group, spec.denyShort) {
				return nil, "the option " + a + " needs approval"
			}
			mapped = append(mapped, a)
			for j := 0; j < len(group); j++ {
				c := group[j]
				if c >= '0' && c <= '9' {
					break // head -5
				}
				if strings.IndexByte(spec.valueFlags, c) >= 0 {
					if j == len(group)-1 && i+1 < len(args) {
						i++
						mapped = append(mapped, args[i])
					}
					break
				}
			}
		default:
			mapped = append(mapped, remotePath(a))
		}
	}
	return mapped, ""
}

// remotePath maps a leading ~ to the home folder (the command runs there and
// argv has no shell). Every other path, including "..", absolute and hidden
// ones, is passed on as given: the cluster's permissions decide.
func remotePath(a string) string {
	switch {
	case a == "~":
		return "."
	case strings.HasPrefix(a, "~/"):
		return "./" + a[2:]
	}
	return a
}
