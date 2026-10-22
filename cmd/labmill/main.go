// Command labmill is a conversational front end to Slurm clusters. See README.md for what it does and CLAUDE.md for the design.
//
// Subcommands:
//
//	Labmill                          start the terminal interface (first run: setup)
//	labmill version                  print the version
//	Labmill shell [--dir <folder>] <cluster>
//	                               a terminal on the cluster's login node (no new sign-in)
//	labmill ssh-helper               run the shared SSH connection helper (started automatically)
//	labmill ssh-exec <alias> [-s <subsystem> | <command>...]
//	                               bridge stdio to a host through the helper (used by rclone)
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	"labmill-client/internal/launch"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	context.AfterFunc(ctx, stop) // after the first Ctrl-C, a second one kills the process
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run dispatches a subcommand and returns the process exit code. It never
// calls os.Exit itself, so it can be tested.
//   - no args: runTUI
//   - "version", "--version": print "Labmill <version>"
//   - "ssh-helper", "ssh-exec": launch.RunSubcommand (shared with the GUI)
//     (ssh-exec exit code = remote exit code;
//     255 when it couldn't connect)
//   - "shell": launch.RunShell (exit code = the shell's)
//   - "help", "-h", "--help": usage on stdout, 0
//   - anything else: "unknown command" plus usage on stderr, 2
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return runTUI(ctx, stderr)
	}
	switch args[0] {
	case "version", "--version":
		fmt.Fprintf(stdout, "Labmill %s\n", version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "shell":
		return launch.RunShell(ctx, args[1:], stdin, stdout, stderr)
	}
	if launch.HelperSubcommand(args) {
		return launch.RunSubcommand(ctx, args, stdin, stdout, stderr)
	}
	fmt.Fprintf(stderr, "Labmill: unknown command %q\n\n%s", args[0], usage)
	return 2
}

// usage is the help text.
const usage = `labmill - describe it, yeet it, compute it

Usage:
  Labmill                start Labmill
  labmill version        print the version
  Labmill shell <cluster>  open a terminal on the cluster's login node
                         (--dir <folder> starts in that folder)
  labmill help           show this help

Internal commands (used by Labmill itself):
  labmill ssh-helper     keep your cluster login open for a few minutes
  labmill ssh-exec       connect a file transfer through that login
`
