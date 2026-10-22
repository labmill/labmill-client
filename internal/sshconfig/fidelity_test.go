package sshconfig

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLookupProxyJumpNone(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), "Host direct\n    ProxyJump none\n\nHost *\n    ProxyJump bastion\n")
	h, _, err := Lookup(p, "direct")
	if err != nil || h.ProxyJump != "" {
		t.Errorf("direct: %+v %v", h, err)
	}
	if o, _, _ := Lookup(p, "other"); o.ProxyJump != "bastion" {
		t.Errorf("other: %+v", o)
	}
}

func TestLookupHostNameTokens(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), "Host n1 n2\n    HostName %h.example.edu\n\nHost pct\n    HostName 100%%.example.edu\n")
	if h, _, _ := Lookup(p, "n2"); h.HostName != "n2.example.edu" {
		t.Errorf("%%h: %q", h.HostName)
	}
	if h, _, _ := Lookup(p, "pct"); h.HostName != "100%.example.edu" {
		t.Errorf("%%%%: %q", h.HostName)
	}
}

// OpenSSH: Host patterns are case-sensitive, Match host lists are not
// (verified with ssh -G on OpenSSH 9.6).
func TestLookupHostCaseSensitiveMatchHostNot(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), "Host Kestrel\n    User bob\n\nHost *.Example.EDU\n    Port 2200\n\nHost * !Secure\n    ProxyJump bastion\n")
	if h, _, _ := Lookup(p, "kestrel"); h.User == "bob" {
		t.Errorf("Host Kestrel matched kestrel: %+v", h)
	}
	if h, _, _ := Lookup(p, "Kestrel"); h.User != "bob" {
		t.Errorf("Host Kestrel didn't match Kestrel: %+v", h)
	}
	if h, _, _ := Lookup(p, "FOO.example.edu"); h.Port == 2200 {
		t.Errorf("case-different wildcard matched: %+v", h)
	}
	if h, _, _ := Lookup(p, "secure"); h.ProxyJump != "bastion" {
		t.Errorf("!Secure negated lowercase secure: %+v", h)
	}
	q := tempPaths(t)
	writeFile(t, q.UserConfig(), "Match host *,!SECURE\n    ProxyJump bastion\n")
	if h, _, _ := Lookup(q, "secure"); h.ProxyJump != "" {
		t.Errorf("Match host negation should fold case: %+v", h)
	}
	if h, _, _ := Lookup(q, "other"); h.ProxyJump != "bastion" {
		t.Errorf("Match host *: %+v", h)
	}
}

func TestLookupAllIdentityFiles(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), "Host a\n    IdentityFile ~/k1\n    IdentityFile ~/k2\n\nHost *\n    IdentityFile ~/k3\n")
	h, _, err := Lookup(p, "a")
	want := []string{"~/k1", "~/k2", "~/k3"}
	if err != nil || !reflect.DeepEqual(h.IdentityFiles, want) || h.IdentityFile != "~/k1" {
		t.Errorf("%+v %v", h, err)
	}
	// Render writes all of them.
	out := string(Render([]Host{{Alias: "a", IdentityFiles: []string{"/a", "/b c"}}}, "linux"))
	if want := "    IdentityFile /a\n    IdentityFile \"/b c\"\n"; !strings.Contains(out, want) {
		t.Errorf("render: %s", out)
	}
}

func TestMatchHostUsesCanonicalHostName(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), "Host a\n    HostName real.example.edu\n\nMatch host real.example.edu\n    User bycanon\n\nMatch host a\n    Port 2200\n\nMatch host Nohost\n    ProxyJump no\n")
	h, _, err := Lookup(p, "a")
	if err != nil || h.User != "bycanon" {
		t.Errorf("canonical match: %+v %v", h, err)
	}
	if h.Port != 22 {
		t.Errorf("alias must not match once HostName is set: %+v", h)
	}
	// Without a HostName the alias is used (case-insensitively).
	if o, _, _ := Lookup(p, "NOHOST"); o.ProxyJump != "no" {
		t.Errorf("fallback to alias: %+v", o)
	}
}

func TestUnnormalizableHostScopesIncludeToNothing(t *testing.T) {
	for _, line := range []string{`Host "a b"`, "Host"} {
		p := tempPaths(t)
		writeFile(t, p.UserConfig(), line+"\n    Include extra\n    Port 2222\n")
		writeFile(t, filepath.Join(p.SSHDir, "extra"), "User leaked\n")
		for _, a := range []string{"a", "b", "x"} {
			if h, _, _ := Lookup(p, a); h.User != "" || h.Port != 22 {
				t.Errorf("%q: %s leaked: %+v", line, a, h)
			}
		}
	}
}

// A Host/Match line inside an included file is evaluated normally, but only
// while the Include's enclosing block matches the host. Ground truth below was
// observed with `ssh -G -F cfg <host>` on this box's OpenSSH (readconf.c);
// "inc" files are written next to the config and included by path.
//
//	inc:  User incuser / Host * Port 2201 / Host a ProxyJump ja /
//	      Host b IdentityFile /k/b / Match host a User matchuser
//	ca:   Host a { Include inc; HostName after.example }  Host b2 { Include inc }
//	  ca a   -> user incuser, hostname after.example, port 2201, proxyjump ja
//	  ca b2  -> user incuser, hostname b2, port 2201 (Host a / Host b blocks of inc don't match)
//	  ca b   -> user dp(default), port 22 (outer block doesn't match: nothing of inc)
//	cb:   Host b { Include inc }  Host a { Port 9 }
//	  cb a   -> default user, port 9, no proxyjump (inc not processed, even its Host a)
//	  cb b   -> user incuser, port 2201, identityfile /k/b
//	cc:   Host a { Include inc2; ProxyJump afterinc }  Host * { Port 7 }
//	inc2: Host b { User buser }
//	  cc a   -> default user, port 7, proxyjump afterinc (directive after the Include applies again)
//	  cc b   -> default user, port 7
//	cd:   Host a { HostName real.example; Include incm }
//	incm: Match host real.example Port 2299 / Match host a ProxyJump byalias / Match all User all
//	  cd a   -> user all, hostname real.example, port 2299, no proxyjump
//	             (Match host sees the HostName resolved so far, not the alias)
//	  cd z   -> all defaults (outer block doesn't match)
//	ce:   Host a { Include ince; User after }  Host b { Include ince }
//	ince: Match host a ProxyJump viamatch / Match host zzz Port 5
//	  ce a   -> user after, port 22, proxyjump viamatch
//	  ce b   -> all defaults
func TestIncludedHostMatchLinesFollowOuterBlock(t *testing.T) {
	files := map[string]string{
		"inc":  "User incuser\nHost *\n  Port 2201\nHost a\n  ProxyJump ja\nHost b\n  IdentityFile /k/b\nMatch host a\n  User matchuser\n",
		"inc2": "Host b\n  User buser\n",
		"incm": "Match host real.example\n  Port 2299\nMatch host a\n  ProxyJump byalias\nMatch all\n  User all\n",
		"ince": "Match host a\n  ProxyJump viamatch\nMatch host zzz\n  Port 5\n",
		"ca":   "Host a\n  Include inc\n  HostName after.example\nHost b2\n  Include inc\n",
		"cb":   "Host b\n  Include inc\nHost a\n  Port 9\n",
		"cc":   "Host a\n  Include inc2\n  ProxyJump afterinc\nHost *\n  Port 7\n",
		"cd":   "Host a\n  HostName real.example\n  Include incm\n",
		"ce":   "Host a\n  Include ince\n  User after\nHost b\n  Include ince\n",
	}
	tests := []struct {
		cfg, host string
		want      Host
	}{
		{"ca", "a", Host{HostName: "after.example", User: "incuser", Port: 2201, ProxyJump: "ja"}},
		{"ca", "b2", Host{HostName: "b2", User: "incuser", Port: 2201}},
		{"ca", "b", Host{HostName: "b", Port: 22}},
		{"cb", "a", Host{HostName: "a", Port: 9}},
		{"cb", "b", Host{HostName: "b", User: "incuser", Port: 2201, IdentityFile: "/k/b", IdentityFiles: []string{"/k/b"}}},
		{"cc", "a", Host{HostName: "a", Port: 7, ProxyJump: "afterinc"}},
		{"cc", "b", Host{HostName: "b", Port: 7}},
		{"cd", "a", Host{HostName: "real.example", User: "all", Port: 2299}},
		{"cd", "z", Host{HostName: "z", Port: 22}},
		{"ce", "a", Host{HostName: "a", User: "after", Port: 22, ProxyJump: "viamatch"}},
		{"ce", "b", Host{HostName: "b", Port: 22}},
	}
	for _, tc := range tests {
		t.Run(tc.cfg+"_"+tc.host, func(t *testing.T) {
			p := tempPaths(t)
			for name, body := range files {
				writeFile(t, filepath.Join(p.SSHDir, name), body)
			}
			writeFile(t, p.UserConfig(), files[tc.cfg])
			got, _, err := Lookup(p, tc.host)
			if err != nil {
				t.Fatal(err)
			}
			tc.want.Alias = tc.host
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// Host blocks of an included file define no aliases, even when they would
// match: the Include's outer block decides.
func TestIncludedHostBlocksDefineNoAlias(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), "Host a\n    HostName a.example.edu\n    Include extra\n")
	writeFile(t, filepath.Join(p.SSHDir, "extra"), "User fromextra\nHost b\n    User evil\n")
	if h, found, _ := Lookup(p, "b"); h.User != "" || found {
		t.Errorf("b: %+v %v", h, found)
	}
	// UniqueAlias is conservative: a Host pattern in any included file counts
	// as taken, even where it would not apply, so a new "b" never clashes.
	if al, err := UniqueAlias(p, "b"); err != nil || al != "b-2" {
		t.Errorf("unique b: %q %v", al, err)
	}
}

func TestFindAliasSeesHostNameAfterScopedInclude(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), "Host a\n    Include extra\n    HostName after.example.edu\n\nHost b\n    Include extra2\n")
	writeFile(t, filepath.Join(p.SSHDir, "extra"), "User u\n")
	writeFile(t, filepath.Join(p.SSHDir, "extra2"), "HostName inc.example.edu\n")
	if al, ok, err := FindAliasForHostName(p, "after.example.edu"); err != nil || !ok || al != "a" {
		t.Errorf("after: %q %v %v", al, ok, err)
	}
	if al, ok, err := FindAliasForHostName(p, "inc.example.edu"); err != nil || !ok || al != "b" {
		t.Errorf("included: %q %v %v", al, ok, err)
	}
}

func TestValidateIdentityFilesEachElement(t *testing.T) {
	for _, ids := range [][]string{{"ok", ""}, {"ok", "k\nProxyCommand x"}} {
		if err := (Host{Alias: "a", IdentityFiles: ids}).Validate(); err == nil {
			t.Errorf("IdentityFiles %q accepted", ids)
		}
	}
}

func TestLookupHostNameUnknownToken(t *testing.T) {
	for _, hn := range []string{"%p.example.edu", "%r@x", "host%", "%n.example.edu", "%d"} {
		p := tempPaths(t)
		writeFile(t, p.UserConfig(), "Host bad\n    HostName "+hn+"\n")
		if _, _, err := Lookup(p, "bad"); err == nil {
			t.Errorf("HostName %q: no error", hn)
		}
	}
}

// A HostName reached through an Include only counts for an alias when every
// enclosing block matches that alias. Ground truth (ssh -G -F cfg <host>,
// OpenSSH 9.6, absolute Include paths):
//
//	c1:  Host a { Include inc1 }  Host b { User ub }
//	inc1: Host b { Include other1; HostName y.example }
//	  c1 a -> hostname a, user dp;  c1 b -> hostname b, user ub
//	  (inc1's Host b sits inside Host a, so y.example is never applied)
//	cn:  Host a { Include n1 }; n1: Host b { Include n3b }; n3b: HostName fromb
//	  cn a -> hostname a;  cn b -> hostname b
//	  (the innermost HostName needs both Host a and Host b to match)
func TestAliasIgnoresIncludedHostNameOfNonMatchingBlock(t *testing.T) {
	p := tempPaths(t)
	d := p.SSHDir
	writeFile(t, p.UserConfig(), "Host a\n  Include "+d+"/inc1\nHost b\n  User ub\n")
	writeFile(t, filepath.Join(d, "inc1"), "Host b\n  Include "+d+"/other1\n  HostName y.example\n")
	writeFile(t, filepath.Join(d, "other1"), "Port 1\n")
	if al, ok, _ := FindAliasForHostName(p, "y.example"); ok {
		t.Errorf("y.example attributed to %q", al)
	}
	if al, _, ok, _ := AliasSource(p, "y.example"); ok {
		t.Errorf("AliasSource: y.example attributed to %q", al)
	}

	q := tempPaths(t)
	d = q.SSHDir
	writeFile(t, q.UserConfig(), "Host a\n  Include "+d+"/n1\n")
	writeFile(t, filepath.Join(d, "n1"), "Host b\n  Include "+d+"/n3b\n")
	writeFile(t, filepath.Join(d, "n3b"), "HostName fromb\n")
	if al, ok, _ := FindAliasForHostName(q, "fromb"); ok {
		t.Errorf("fromb attributed to %q", al)
	}
	for _, a := range []string{"a", "b"} {
		if h, _, _ := Lookup(q, a); h.HostName != a {
			t.Errorf("%s: %+v", a, h)
		}
	}
}

// An Include at the bottom of the file after "Host *" sits inside that block;
// Host lines of the included file still define aliases. Ground truth:
//
//	cs: Host * { ForwardX11 no }  Include s1;  s1: Host hpcx { HostName login.example.edu }
//	  cs hpcx -> hostname login.example.edu
func TestIncludeInsideHostStarDefinesAliases(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), "Host *\n  ForwardX11 no\nInclude "+p.SSHDir+"/s1\n")
	writeFile(t, filepath.Join(p.SSHDir, "s1"), "Host hpcx\n  HostName login.example.edu\n")
	h, found, err := Lookup(p, "hpcx")
	if err != nil || !found || h.HostName != "login.example.edu" {
		t.Errorf("lookup: %+v %v %v", h, found, err)
	}
	if al, ok, _ := FindAliasForHostName(p, "login.example.edu"); !ok || al != "hpcx" {
		t.Errorf("find: %q %v", al, ok)
	}
	if al, err := UniqueAlias(p, "hpcx"); err != nil || al == "hpcx" {
		t.Errorf("unique: %q %v", al, err)
	}
}

// OpenSSH decides a Match block once, at its Match line; a HostName set later
// inside the block does not re-evaluate it for an Include or for the lines
// after the Include. Ground truth:
//
//	cm: Match host a { HostName real.example; Include m1; User afterinc }  m1: Port 4444
//	  cm a -> hostname real.example, port 4444, user afterinc
func TestMatchDecidedOnceAtItsOwnLine(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), "Match host a\n  HostName real.example\n  Include "+p.SSHDir+"/m1\n  User afterinc\n")
	writeFile(t, filepath.Join(p.SSHDir, "m1"), "Port 4444\n")
	h, _, err := Lookup(p, "a")
	if err != nil || h.HostName != "real.example" || h.Port != 4444 || h.User != "afterinc" {
		t.Errorf("%+v %v", h, err)
	}
	// A block that did not match stays unmatched even if a later HostName would.
	q := tempPaths(t)
	writeFile(t, q.UserConfig(), "Host a\n  HostName z\nMatch host z\n  Include "+q.SSHDir+"/m1\n  User afterinc\n")
	writeFile(t, filepath.Join(q.SSHDir, "m1"), "Port 4444\n")
	if h, _, _ := Lookup(q, "a"); h.Port != 4444 || h.User != "afterinc" {
		t.Errorf("match on canonical name: %+v", h)
	}
}
