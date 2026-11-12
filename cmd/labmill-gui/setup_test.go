//go:build desktop

package main

import (
	"strings"
	"testing"

	"labmill-client/internal/site"
)

// The setup form can't finish without a home institution (M55): an empty or
// unknown choice is refused, and so is the "not listed" institution with
// no login host. Nothing reaches Build then.
func TestSubmitSetupNeedsHomeInstitution(t *testing.T) {
	for _, tc := range []struct {
		name, domain, login, want string
	}{
		{"empty choice", "", "", "home institution"},
		{"blank choice", "  ", "", "home institution"},
		{"unknown", "nowhere.example", "", "choose your institution"},
		{"generic, no hosts", site.GenericDomain, "", "login host of your institution"},
		{"bad user", site.GenericDomain, "login.example.edu", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSetup(true)
			name := "alice"
			if tc.name == "bad user" {
				name = "Bad Name"
			}
			err := s.SubmitSetup(tc.domain, name, tc.login)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("SubmitSetup = %v, want %q", err, tc.want)
			}
			if !s.NeedsSetup() {
				t.Error("setup counted as done after a refused submit")
			}
			select {
			case a := <-s.answers:
				t.Errorf("answers sent: %+v", a)
			default:
			}
		})
	}
}

func TestSubmitSetupGenericWithHost(t *testing.T) {
	s := newSetup(true)
	if err := s.SubmitSetup(site.GenericDomain, "alice", "login.example.edu"); err != nil {
		t.Fatal(err)
	}
	a := <-s.answers
	if a.Site != site.GenericDomain || a.LoginHost != "login.example.edu" {
		t.Errorf("answers = %+v", a)
	}
	if s.NeedsSetup() {
		t.Error("still needs setup")
	}
}

func TestSetupNoteInState(t *testing.T) {
	s := newSetup(true)
	s.setNote("choose your home institution")
	if st := s.State(); st.Phase != "setup" || st.Note == "" {
		t.Errorf("state = %+v", st)
	}
}

// With clusters already configured only the institution is asked: no
// account name or hosts are needed.
func TestSubmitSetupOnlyInstitution(t *testing.T) {
	s := newSetup(true)
	s.setOnlyInstitution(true)
	if err := s.SubmitSetup("", "", ""); err == nil {
		t.Error("empty institution accepted")
	}
	if err := s.SubmitSetup(site.GenericDomain, "", ""); err != nil {
		t.Fatal(err)
	}
	if a := <-s.answers; a.Site != site.GenericDomain || a.Username != "" {
		t.Errorf("answers = %+v", a)
	}
}

// An institution from the account's list is sent with the answers; the profile is the generic one when this build
// has none for its domain. An empty id is "my institution isn't listed".
func TestSubmitHome(t *testing.T) {
	s := newSetup(true)
	if err := s.SubmitHome("i1", "University of Oregon", "uoregon.edu", "US", "alice", "login.uoregon.edu"); err != nil {
		t.Fatal(err)
	}
	a := <-s.answers
	if a.Site != site.GenericDomain || a.Home == nil || a.Home.ID != "i1" || a.LoginHost != "login.uoregon.edu" {
		t.Errorf("answers = %+v", a)
	}
	s = newSetup(true)
	if err := s.SubmitHome("", "", "", "", "alice", "login.example.edu"); err != nil {
		t.Fatal(err)
	}
	if a := <-s.answers; a.Home != nil || a.Site != site.GenericDomain {
		t.Errorf("not listed: %+v", a)
	}
	if err := newSetup(true).SubmitHome("i1", "X", "x.edu", "USA", "alice", "login.x.edu"); err == nil {
		t.Error("a bad country code was accepted")
	}
}

// Without a connected account the search refuses, and the state says so.
func TestSearchNeedsSignIn(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	s := newSetup(true)
	if st := s.State(); st.SignedIn {
		t.Errorf("signed in without account.json: %+v", st)
	}
	if _, err := s.SearchInstitutions("oregon"); err == nil {
		t.Error("search without sign-in")
	}
	if l, err := s.SearchInstitutions("o"); err != nil || len(l) != 0 {
		t.Errorf("one letter: %v %v", l, err)
	}
}
