package launch

import (
	"net/url"
	"strings"
	"testing"

	"labmill-client/internal/account"
	"labmill-client/internal/mailreq"
)

func req(r account.AccountRequest) account.System {
	return account.System{Institution: "Example University", AccountRequest: &r}
}

func TestBuildAccountRequest(t *testing.T) {
	full := account.AccountRequest{To: "help@example.edu", SubjectTemplate: "Account for {name}", BodyTemplate: "I am {name} at {institution}. {reason}", WebFormURL: "https://example.edu/form"}
	d, err := buildAccountRequest(req(full), "Ann Lee", "RNA-seq", "example.edu")
	if err != nil || !strings.HasPrefix(d.OpenURL, "mailto:help@example.edu?") || d.Subject != "Account for Ann Lee" || !strings.Contains(d.Body, "at Example University. RNA-seq") || d.Warning != "" {
		t.Fatalf("mail draft: %+v %v", d, err)
	}
	if _, err := buildAccountRequest(account.System{}, "Ann", "x", ""); err == nil {
		t.Error("no request path: want error")
	}
}

func TestBuildAccountRequestFormAndDefaults(t *testing.T) {
	// No address: the form.
	d, _ := buildAccountRequest(req(account.AccountRequest{WebFormURL: "https://example.edu/form"}), "Ann", "x", "example.edu")
	if d.OpenURL != "https://example.edu/form" || d.To != "" {
		t.Errorf("form: %+v", d)
	}
	// An address without templates: neutral subject and body.
	d, _ = buildAccountRequest(req(account.AccountRequest{To: "help@example.edu"}), "Ann", "need GPUs", "example.edu")
	if d.Subject != "Account request: Ann" || !strings.Contains(d.Body, "I am Ann at Example University") || !strings.Contains(d.Body, "need GPUs") {
		t.Errorf("defaults: %+v", d)
	}
	// A bad address falls back to the form.
	d, _ = buildAccountRequest(req(account.AccountRequest{To: "a@b.edu,evil@x.org", WebFormURL: "https://example.edu/form"}), "Ann", "x", "example.edu")
	if d.OpenURL != "https://example.edu/form" || d.To != "" {
		t.Errorf("bad address: %+v", d)
	}
}

func TestBuildAccountRequestRefusesBadTargets(t *testing.T) {
	for _, to := range []string{"help@example.edu?cc=evil@x.org", "help@example.edu%0Abcc:evil@x.org", "help@example.edu\r\nBcc: evil@x.org"} {
		d, _ := buildAccountRequest(req(account.AccountRequest{To: to}), "Ann", "x", "example.edu")
		if d.OpenURL != "" || d.To != "" {
			t.Errorf("to %q opened: %+v", to, d)
		}
	}
	for _, f := range []string{"javascript:alert(1)", "file:///etc/passwd", "http://example.edu/form", "data:text/html,x"} {
		d, _ := buildAccountRequest(req(account.AccountRequest{WebFormURL: f}), "Ann", "x", "example.edu")
		if d.OpenURL != "" || d.WebForm != "" {
			t.Errorf("form %q opened: %+v", f, d)
		}
	}
}

func TestBuildAccountRequestInjectionStaysData(t *testing.T) {
	r := account.AccountRequest{To: "help@example.edu", SubjectTemplate: "Hi\r\nBcc: evil@x.org {name}", BodyTemplate: "Line\r\n&bcc=evil@x.org&cc=a@b.c {reason}"}
	// {reason} inside the user's name and the institution is not filled again.
	sys := account.System{Institution: "{reason} U", AccountRequest: &r}
	d, _ := buildAccountRequest(sys, "{reason}", "REAL", "example.edu")
	if strings.ContainsAny(d.Subject, "\r\n") || d.Subject != "Hi Bcc: evil@x.org {reason}" {
		t.Errorf("subject: %q", d.Subject)
	}
	u, err := url.Parse(d.OpenURL)
	if err != nil || u.Opaque == "" && u.Path == "" {
		t.Fatalf("link: %q %v", d.OpenURL, err)
	}
	q := u.Query()
	if len(q) != 2 || q.Get("bcc") != "" || q.Get("cc") != "" || q.Get("subject") != d.Subject || q.Get("body") != d.Body || !strings.Contains(d.Body, "&bcc=evil@x.org") || !strings.HasSuffix(d.Body, "REAL") {
		t.Errorf("link query %v, body %q", q, d.Body)
	}
}

func TestBuildAccountRequestLongBody(t *testing.T) {
	long := strings.Repeat("x", mailreq.MaxBody+10)
	d, _ := buildAccountRequest(req(account.AccountRequest{To: "help@example.edu", SubjectTemplate: "S", BodyTemplate: "{reason}"}), "Ann", long, "example.edu")
	if d.Clipboard != long || d.Body != mailreq.LongBodyNote || strings.Contains(d.OpenURL, "xxxx") {
		t.Errorf("long body: body %q, clip %d, link %.60s", d.Body, len(d.Clipboard), d.OpenURL)
	}
}

func TestForeignWarning(t *testing.T) {
	r := account.AccountRequest{To: "help@other.org", WebFormURL: "https://forms.example.edu/x"}
	d, _ := buildAccountRequest(req(r), "Ann", "x", "example.edu")
	if !strings.Contains(d.Warning, "other.org") {
		t.Errorf("foreign address: %q", d.Warning)
	}
	r.To = ""
	d, _ = buildAccountRequest(req(r), "Ann", "x", "example.edu")
	if d.Warning != "" || d.OpenURL == "" {
		t.Errorf("subdomain form: %+v", d)
	}
	r.WebFormURL = "https://example.edu.evil.org/x"
	if d, _ = buildAccountRequest(req(r), "Ann", "x", "example.edu"); d.Warning == "" {
		t.Error("look-alike host: want a warning")
	}
}
