package launch

import (
	"context"
	"fmt"
	"io"
	"strings"

	"labmill-client/internal/config"
	"labmill-client/internal/site"
	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx"
)

// Every Labmill front end re-runs its own executable for two helper jobs: the
// shared SSH connection helper (`<exe> ssh-helper`, started by sshx when a
// login is needed) and rclone's SSH transport (`<exe> ssh-exec <alias> …`).
// So each binary, the TUI and the GUI alike, must dispatch these before
// starting its interface.

// HelperSubcommand reports whether args (os.Args[1:]) ask for one of the
// helper subcommands that every Labmill binary must handle.
func HelperSubcommand(args []string) bool {
	return len(args) > 0 && (args[0] == "ssh-helper" || args[0] == "ssh-exec")
}

// RunSubcommand runs a helper subcommand (see HelperSubcommand) and returns
// the process exit code.
func RunSubcommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	switch args[0] {
	case "ssh-helper":
		return RunHelper(ctx, stderr)
	case "ssh-exec":
		return RunSSHExec(ctx, args[1:], stdin, stdout, stderr)
	}
	fmt.Fprintf(stderr, "Labmill: unknown helper command %q\n", args[0])
	return 2
}

// RunHelper runs sshx.Serve with the default socket path, the user's SSH
// config paths, and LoginOptions from the sites in clusters.yaml (currentLoginOptions).
// "already running" is exit code 0 (another helper serves the user).
func RunHelper(ctx context.Context, stderr io.Writer) int {
	sock, err := sshx.SocketPath()
	if err != nil {
		fmt.Fprintf(stderr, "Labmill: %v\n", err)
		return 1
	}
	paths, err := sshconfig.DefaultPaths()
	if err != nil {
		fmt.Fprintf(stderr, "Labmill: %v\n", err)
		return 1
	}
	err = sshx.Serve(ctx, sshx.HelperOptions{
		SocketPath: sock,
		Paths:      paths,
		Login:      currentLoginOptions(),
		// Read per login, so a cluster of another institution added while
		// the helper runs gets its own institution's options.
		LoginFor: currentLoginOptions,
	})
	switch {
	case err == nil, ctx.Err() != nil:
		return 0
	case strings.Contains(err.Error(), "already running"):
		return 0
	}
	fmt.Fprintf(stderr, "Labmill: ssh helper stopped: %v\n", err)
	return 1
}

// RunSSHExec runs sshx.Exec for `ssh-exec <alias> args...` with stdio and
// returns the remote exit code (255 and a message on stderr if it couldn't
// connect, e.g. "not logged in").
func RunSSHExec(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, "labmill ssh-exec: missing host name\n")
		return 2
	}
	code, err := sshx.Exec(ctx, args[0], args[1:], stdin, stdout, stderr, sshx.ExecOptions{})
	if err != nil {
		fmt.Fprintf(stderr, "labmill ssh-exec: %v\n", err)
		return 255
	}
	return code
}

// currentLoginOptions reads clusters.yaml and builds the login options for
// its home institution and the other institutions its clusters name
// (loginOptionsFor): each host shows its own institution's 2FA help and key
// rules. Without a readable config it is the default site's options.
func currentLoginOptions() sshx.LoginOptions {
	var cfg *config.Config
	if p, err := config.DefaultPath(); err == nil {
		cfg, _ = config.Load(p)
	}
	home, _ := site.ForConfig(cfg)
	return loginOptionsFor(sitesOfConfig(home, cfg, othersForSite(home.Domain)))
}
