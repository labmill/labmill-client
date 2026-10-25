package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"labmill-client/internal/account"
	"labmill-client/internal/store"
)

// runAccount handles "login", "logout" and "whoami" (the Labmill account
// link, internal/account). The token is read from stdin (hidden on a
// terminal), never from the command line, where other users and the shell
// history could see it.
func runAccount(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) int {
	path, err := account.Path()
	if err != nil {
		fmt.Fprintln(stderr, "Labmill:", err)
		return 1
	}
	switch cmd {
	case "login":
		base, err := account.BaseURL()
		if err != nil {
			fmt.Fprintln(stderr, "Labmill:", err)
			return 1
		}
		fmt.Fprintf(stderr, "Make a program token at %s (Settings > Program tokens) and paste it here: ", base)
		tok, err := readToken(stdin)
		fmt.Fprintln(stderr)
		if err != nil {
			fmt.Fprintln(stderr, "Labmill: no token read:", err)
			return 1
		}
		me, err := account.Login(ctx, path, base, tok, nil, time.Now())
		if err != nil {
			fmt.Fprintln(stderr, "Labmill:", err)
			return 1
		}
		fmt.Fprintf(stdout, "Connected to the Labmill account of %s. Jobs that end from now on are sent to it.\n", who(me))
		return 0
	case "logout":
		if err := account.Remove(path); err != nil {
			fmt.Fprintln(stderr, "Labmill:", err)
			return 1
		}
		fmt.Fprintln(stdout, "Disconnected. Labmill sends nothing to a Labmill account now. Revoke the token at app.labmill.org if you no longer need it.")
		return 0
	default: // whoami
		me, l, err := account.Whoami(ctx, path, nil)
		if errors.Is(err, account.ErrNotLinked) {
			fmt.Fprintln(stdout, "Not connected to a Labmill account. Nothing is sent. To connect: labmill login")
			return 1
		}
		if err != nil {
			fmt.Fprintln(stderr, "Labmill:", err)
			return 1
		}
		fmt.Fprintf(stdout, "Connected to %s as %s, since %s.\n", l.URL, who(me), l.LinkedAt.Local().Format("2006-01-02 15:04"))
		if dbPath, err := store.DefaultPath(); err == nil {
			if _, statErr := os.Stat(dbPath); statErr == nil {
				if st, err := store.Open(dbPath); err == nil {
					sent, waiting, refused, last, err := account.Queue(ctx, st)
					st.Close()
					if err == nil {
						fmt.Fprintf(stdout, "Job reports: %d sent, %d waiting, %d refused by the account.\n", sent, waiting, refused)
						if last != "" {
							fmt.Fprintln(stdout, "Last problem:", last)
						}
					}
				}
			}
		}
		return 0
	}
}

func who(me account.Me) string {
	if me.DisplayName != "" {
		return me.DisplayName
	}
	return me.PersonID
}

// readToken reads one line, without echo when stdin is a terminal.
func readToken(stdin io.Reader) (string, error) {
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		b, err := term.ReadPassword(int(f.Fd()))
		return strings.TrimSpace(string(b)), err
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
