package mailreq

import (
	"net/url"
	"strings"
	"testing"
)

func TestMailtoShort(t *testing.T) {
	d, err := Mailto("help@example.edu", "Account request\nBcc: x", "Hello,\nplease add me.")
	if err != nil || d.Clipboard != "" {
		t.Fatalf("%+v %v", d, err)
	}
	u, err := url.Parse(d.URL)
	if err != nil || u.Scheme != "mailto" || u.Opaque != "help@example.edu" {
		t.Fatalf("url %q: %v", d.URL, err)
	}
	if s := u.Query().Get("subject"); s != "Account request Bcc: x" {
		t.Errorf("subject = %q", s)
	}
	if b := u.Query().Get("body"); b != "Hello,\nplease add me." {
		t.Errorf("body = %q", b)
	}
	if strings.Contains(d.URL, "+") {
		t.Errorf("plus sign in %q", d.URL)
	}
}

func TestMailtoLongGoesToClipboard(t *testing.T) {
	long := strings.Repeat("x", MaxBody+1)
	d, err := Mailto("help@example.edu", "s", long)
	if err != nil || d.Clipboard != long || len(d.URL) > 400 {
		t.Fatalf("clip=%d url=%d %v", len(d.Clipboard), len(d.URL), err)
	}
	if d, _ := Mailto("help@example.edu", "s", strings.Repeat("x", MaxBody)); d.Clipboard != "" {
		t.Error("body at the limit moved to clipboard")
	}
}

func TestMailtoRefusesBadAddress(t *testing.T) {
	for _, to := range []string{"", "a@b.edu,c@d.edu", "Bob <b@x.edu>", "a@b.edu?cc=x@y.z", "nobody"} {
		if _, err := Mailto(to, "s", "b"); err == nil {
			t.Errorf("%q accepted", to)
		}
	}
}

func TestOpener(t *testing.T) {
	for goos, want := range map[string]string{"linux": "xdg-open", "darwin": "/usr/bin/open", "windows": `C:\Windows\System32\rundll32.exe`} {
		name, args, err := Opener(goos, "mailto:a@b.edu?subject=x")
		if err != nil || name != want || args[len(args)-1] != "mailto:a@b.edu?subject=x" {
			t.Errorf("%s: %q %v %v", goos, name, args, err)
		}
	}
	for _, bad := range []string{"file:///etc/passwd", "http://x.edu", "javascript:1", "https://x.edu/\nfoo", "ssh://x"} {
		if _, _, err := Opener("linux", bad); err == nil {
			t.Errorf("%q allowed", bad)
		}
	}
}

func TestOpenStarts(t *testing.T) {
	var got []string
	old := start
	start = func(name string, args ...string) error { got = append([]string{name}, args...); return nil }
	defer func() { start = old }()
	if err := Open("https://help.example.edu/form"); err != nil || got[len(got)-1] != "https://help.example.edu/form" {
		t.Errorf("%v %v", got, err)
	}
}

func TestFill(t *testing.T) {
	got := Fill("Hi {name} at {institution}: {reason} {other}", "Ann", "{reason}", "need GPUs")
	if got != "Hi Ann at {reason}: need GPUs {other}" {
		t.Errorf("Fill = %q", got)
	}
}
