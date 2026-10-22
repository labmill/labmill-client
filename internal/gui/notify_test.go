package gui

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestUnixNotify(t *testing.T) {
	var got [][]string
	run, look := runCommand, lookPath
	defer func() { runCommand, lookPath = run, look }()
	runCommand = func(name string, args ...string) error {
		got = append(got, append([]string{name}, args...))
		return nil
	}
	lookPath = func(string) (string, error) { return "/usr/bin/notify-send", nil }
	evil := `x" & do shell script "id" & "`
	unixNotify("darwin", "labmill", evil)
	if len(got) != 1 || got[0][0] != "osascript" || got[0][len(got[0])-1] != evil {
		t.Fatalf("darwin: %v", got)
	}
	for _, a := range got[0][1 : len(got[0])-2] {
		if strings.Contains(a, "shell script") {
			t.Errorf("text leaked into the script: %q", a)
		}
	}
	got = nil
	unixNotify("linux", "labmill", "-bad completed")
	if want := []string{"notify-send", "-a", "labmill", "--", "labmill", "-bad completed"}; len(got) != 1 || !slices.Equal(got[0], want) {
		t.Errorf("linux: %v", got)
	}
	got = nil
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	unixNotify("linux", "labmill", "x")
	unixNotify("freebsd", "labmill", "x")
	if got != nil {
		t.Errorf("nothing should run: %v", got)
	}
}

func TestCleanNotify(t *testing.T) {
	if got := cleanNotify("a\x1b[31m\nb\tc"); strings.ContainsAny(got, "\x1b\n\t") {
		t.Errorf("not clean: %q", got)
	}
	if got := cleanNotify(strings.Repeat("x", 500)); len([]rune(got)) != maxNotifyRunes {
		t.Errorf("length %d", len([]rune(got)))
	}
}
