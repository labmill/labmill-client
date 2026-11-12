package launch

import (
	"strings"
	"testing"

	"labmill-client/internal/account"
)

func TestBuildAccountRequest(t *testing.T) {
	req := &account.AccountRequest{To: "help@example.edu", SubjectTemplate: "Account for {name}", BodyTemplate: "I am {name} at {institution}. {reason}", WebFormURL: "https://example.edu/form"}
	sys := account.System{Institution: "Example University", AccountRequest: req}
	d, err := buildAccountRequest(sys, "Ann Lee", "RNA-seq")
	if err != nil || !strings.HasPrefix(d.OpenURL, "mailto:help@example.edu?") || d.Subject != "Account for Ann Lee" || !strings.Contains(d.Body, "at Example University. RNA-seq") {
		t.Fatalf("mail draft: %+v %v", d, err)
	}
	// No template: the form.
	req2 := *req
	req2.BodyTemplate = ""
	d, _ = buildAccountRequest(account.System{AccountRequest: &req2}, "Ann", "x")
	if d.OpenURL != "https://example.edu/form" || d.To != "" {
		t.Errorf("form: %+v", d)
	}
	// A bad address falls back to the form; a form that is not https is not opened.
	req3 := *req
	req3.To = "a@b.edu,evil@x.org"
	d, _ = buildAccountRequest(account.System{AccountRequest: &req3}, "Ann", "x")
	if d.OpenURL != "https://example.edu/form" || d.To != "" {
		t.Errorf("bad address: %+v", d)
	}
	req4 := account.AccountRequest{URL: "https://example.edu/accounts", WebFormURL: "http://example.edu/form"}
	d, _ = buildAccountRequest(account.System{AccountRequest: &req4}, "Ann", "x")
	if d.OpenURL != "" {
		t.Errorf("http form opened: %+v", d)
	}
	if _, err := buildAccountRequest(account.System{}, "Ann", "x"); err == nil {
		t.Error("no request path: want error")
	}
}
