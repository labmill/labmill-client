// Package mailreq prepares an account request for the user to send
// themselves: a mailto: link for the user's own mail program, or the site's
// web form in the browser. Labmill never sends mail.
package mailreq

import (
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"os/exec"
	"runtime"
	"strings"

	"labmill-client/internal/textsafe"
)

// MaxBody is the longest mailto: body in characters. A longer text goes to
// the clipboard (Draft.Clipboard) and the body only points at it.
const MaxBody = 1500

// Draft is what to show and open for one request.
type Draft struct {
	URL       string // mailto: link to open; "" when only the clipboard text is usable
	Clipboard string // full body when it was too long for the link, else ""
}

// Mailto builds the draft. to must be one plain address; subject and body are
// cleaned of control characters. With a body over MaxBody the link carries a
// one-line note and the full text is returned in Clipboard.
func Mailto(to, subject, body string) (Draft, error) {
	a, err := mail.ParseAddress(to)
	if err != nil || a.Address != to || strings.ContainsAny(to, ",;?&#%") {
		return Draft{}, fmt.Errorf("%q isn't a usable mail address", textsafe.Clean(to))
	}
	subject = oneLine(subject)
	body = textsafe.Clean(body)
	var d Draft
	if len([]rune(body)) > MaxBody {
		d.Clipboard = body
		body = "(The full text is on your clipboard: paste it here.)"
	}
	q := url.Values{}
	q.Set("subject", subject)
	q.Set("body", body)
	// url.Values encodes a space as "+", which mail programs show literally.
	enc := strings.ReplaceAll(q.Encode(), "+", "%20")
	d.URL = "mailto:" + to + "?" + enc
	return d, nil
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(textsafe.Clean(s)), " ")
}

// Opener returns the program and arguments that open target on goos. Only
// mailto: and https: links are allowed; no shell is involved.
func Opener(goos, target string) (string, []string, error) {
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "mailto" && u.Scheme != "https") || strings.ContainsAny(target, "\r\n\x00") {
		return "", nil, errors.New("only mailto: and https: links can be opened")
	}
	switch goos {
	case "windows":
		return `C:\Windows\System32\rundll32.exe`, []string{"url.dll,FileProtocolHandler", target}, nil
	case "darwin":
		return "/usr/bin/open", []string{target}, nil
	default:
		return "xdg-open", []string{target}, nil
	}
}

// start starts a program without waiting for it; tests replace it.
var start = func(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() //nolint:errcheck // the exit status means nothing here
	return nil
}

// Open opens target in the user's mail program or browser. The user presses
// send.
func Open(target string) error {
	name, args, err := Opener(runtime.GOOS, target)
	if err != nil {
		return err
	}
	if err := start(name, args...); err != nil {
		return fmt.Errorf("I couldn't open your mail program or browser: %w", err)
	}
	return nil
}
