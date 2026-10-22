package launch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"labmill-client/internal/config"
	"labmill-client/internal/sshx"
	"labmill-client/internal/store"
)

// ShellNote is written to the terminal when a shell opens from inside the
// TUI.
const ShellNote = "Type exit to come back to Labmill."

// ShellArgs are the parsed arguments of `labmill shell`.
type ShellArgs struct {
	Cluster string // a cluster name from clusters.yaml (or an SSH alias)
	Dir     string // start in this folder on the login node ("" for the home folder)
}

// ParseShellArgs parses `[--dir <path>] <cluster>` (also `--dir=<path>`, in
// either order). Exactly one cluster is required.
func ParseShellArgs(args []string) (ShellArgs, error) {
	var a ShellArgs
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--dir" || arg == "-d":
			if i+1 >= len(args) {
				return a, errors.New("--dir needs a folder")
			}
			i++
			a.Dir = args[i]
		case strings.HasPrefix(arg, "--dir="):
			a.Dir = strings.TrimPrefix(arg, "--dir=")
		case strings.HasPrefix(arg, "-") && arg != "-":
			return a, fmt.Errorf("unknown option %q", arg)
		case a.Cluster != "":
			return a, fmt.Errorf("one cluster at a time (got %q and %q)", a.Cluster, arg)
		default:
			a.Cluster = arg
		}
	}
	if a.Cluster == "" {
		return a, errors.New("missing cluster name")
	}
	if strings.ContainsAny(a.Cluster+a.Dir, "\n\r\x00") {
		return a, errors.New("the cluster and folder can't contain line breaks or NUL")
	}
	return a, nil
}

// ShellAlias returns the SSH alias of the login node for a cluster name in
// cfg (its ssh_host). A name that is no cluster but equals some cluster's
// ssh_host is taken as that alias.
func ShellAlias(cfg *config.Config, name string) (string, error) {
	if cfg == nil {
		return "", errors.New("no clusters are set up yet; start Labmill first")
	}
	if c, ok := cfg.Cluster(name); ok {
		if c.SSHHost == "" {
			return "", fmt.Errorf("cluster %q has no login host (ssh_host)", name)
		}
		return c.SSHHost, nil
	}
	var names []string
	for _, c := range cfg.List() {
		if c.SSHHost == name {
			return name, nil
		}
		names = append(names, c.Name)
	}
	if len(names) == 0 {
		return "", errors.New("no clusters are set up yet; start Labmill first")
	}
	return "", fmt.Errorf("no cluster named %q (known: %s)", name, strings.Join(names, ", "))
}

// OpenShell connects to the cluster's login node through the helper (the
// existing sign-in is reused; otherwise the usual login prompts go through
// the UI, key setup of a key-only host included) and returns the command
// that runs the interactive shell on the terminal. It is for the TUI, which
// runs it with tea.Exec; the connection belongs to the command and is closed
// when it ends. Nothing typed or shown in the shell is logged: labmill.log
// gets "shell opened/closed on <alias>" only.
func (r *Runtime) OpenShell(ctx context.Context, cluster, dir string) (*sshx.ShellExec, error) {
	if r.Env == nil || r.conns == nil {
		return nil, errors.New("shell isn't available here")
	}
	alias, err := ShellAlias(r.Env.Config, cluster)
	if err != nil {
		return nil, err
	}
	cc := r.conns
	// /shell is the user's own request: an earlier refusal after a rejected
	// password (this turn) for this host and its institution is forgotten,
	// but this is still one attempt (M58).
	cc.forgetDenied(alias)
	cl, err := cc.connect(ctx, alias)
	if err != nil {
		if errors.Is(err, sshx.ErrPasswordRejected) {
			cc.markDenied(alias)
		}
		return nil, err
	}
	logPath := r.LogPath
	return &sshx.ShellExec{
		Ctx: ctx, Client: cl, Alias: alias, Dir: dir, Note: ShellNote,
		Logf: func(format string, args ...any) {
			appendLog(logPath, "shell", []string{fmt.Sprintf(format, args...)})
		},
	}, nil
}

// RunShell is `labmill shell <cluster> [--dir <path>]`: an interactive shell
// on the login node over the helper's signed-in connection. Without a login
// the helper holds, it asks for the password and 2FA on the terminal, once
// (a failed password is never retried: many institutions lock the account).
// It returns the remote shell's exit status; 255 and a message on stderr when
// it couldn't connect (like ssh); 2 for bad arguments.
func RunShell(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a, err := ParseShellArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "Labmill shell: %v\nUsage: Labmill shell [--dir <folder>] <cluster>\n", err)
		return 2
	}
	var cfg *config.Config
	if p, perr := config.DefaultPath(); perr == nil {
		cfg, _ = config.Load(p)
	}
	alias, err := ShellAlias(cfg, a.Cluster)
	if err != nil {
		fmt.Fprintf(stderr, "Labmill shell: %v\n", err)
		return 255
	}
	in, ok := stdin.(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		fmt.Fprint(stderr, "Labmill shell: a shell needs an interactive terminal\n")
		return 255
	}
	cl, err := sshConnect(ctx, alias, &termPrompter{in: in, out: stderr}, sshx.ConnectOptions{})
	if err != nil {
		fmt.Fprintf(stderr, "Labmill shell: %v\n", err)
		return 255
	}
	logPath := shellLogPath()
	sh := &sshx.ShellExec{
		Ctx: ctx, Client: cl, Alias: alias, Dir: a.Dir,
		Logf: func(format string, args ...any) {
			appendLog(logPath, "shell", []string{fmt.Sprintf(format, args...)})
		},
	}
	if err := sh.Run(); err != nil {
		var sig *sshx.ErrShellSignaled
		if errors.As(err, &sig) {
			return sig.ExitCode()
		}
		fmt.Fprintf(stderr, "Labmill shell: %v\n", err)
		return 255
	}
	return sh.ExitCode
}

// termPrompter asks the sign-in questions on the terminal for the `labmill
// shell` command. The helper does the login; this relays its questions.
type termPrompter struct {
	in  *os.File
	out io.Writer
}

func (p *termPrompter) Ask(ctx context.Context, q sshx.Question) (string, error) {
	switch q.Kind {
	case sshx.AskKeyPassphrase, sshx.AskKeyCreate, sshx.AskNewPassphrase, sshx.AskKeyUpload, sshx.AskKeyCopy:
		return "", errors.New("this login needs an SSH key set up; start Labmill once to do that")
	}
	prompt := strings.TrimRight(q.Prompt, "\n ")
	if q.Host != "" && !strings.Contains(prompt, q.Host) {
		prompt = q.Host + ": " + prompt
	}
	fmt.Fprint(p.out, prompt+" ")
	fd := int(p.in.Fd())
	saved, serr := term.GetState(fd)
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		if q.Echo || q.Kind == sshx.AskHostKey {
			s, err := readLine(p.in)
			ch <- res{s, err}
			return
		}
		b, err := term.ReadPassword(int(p.in.Fd()))
		fmt.Fprintln(p.out)
		ch <- res{string(b), err}
	}()
	select {
	case r := <-ch:
		return strings.TrimRight(r.s, "\r\n"), r.err
	case <-ctx.Done():
		// The password read never returns now; put the echo back.
		if serr == nil {
			term.Restore(fd, saved)
		}
		return "", ctx.Err()
	}
}

func (p *termPrompter) Notify(msg string) {
	if msg = strings.TrimSpace(msg); msg != "" {
		fmt.Fprintln(p.out, msg)
	}
}

// readLine reads one line byte by byte, so nothing past the newline is
// consumed from the terminal.
func readLine(f io.Reader) (string, error) {
	var sb strings.Builder
	var b [1]byte
	for {
		if _, err := io.ReadFull(f, b[:]); err != nil {
			return sb.String(), err
		}
		if b[0] == '\n' {
			return sb.String(), nil
		}
		sb.WriteByte(b[0])
	}
}

// shellLogPath is labmill.log next to the job database ("" when unknown).
func shellLogPath() string {
	p, err := store.DefaultPath()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(p), "labmill.log")
}

// forgetDenied clears the "password rejected" refusal for alias and the
// other hosts of its institution (the ones markDenied blocked together). The
// refusals of other institutions and the canceled-sign-in flag stay.
func (cc *connCache) forgetDenied(alias string) {
	cc.deniedMu.Lock()
	defer cc.deniedMu.Unlock()
	delete(cc.denied, alias)
	if cc.clusters != nil {
		for _, h := range siblingHosts(cc.clusters(), cc.homeDomain)[alias] {
			delete(cc.denied, h)
		}
	}
}
