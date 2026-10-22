package sshconfig

import "testing"

func TestAliasSource(t *testing.T) {
	p := tempPaths(t)
	writeFile(t, p.UserConfig(), "Host mine\n    HostName a.example.edu\n")
	writeFile(t, p.LabmillConfig(), "Host ours\n    HostName b.example.edu\n")

	a, inY, ok, err := AliasSource(p, "A.example.edu")
	if err != nil || !ok || inY || a != "mine" {
		t.Errorf("user host: %q %v %v %v", a, inY, ok, err)
	}
	a, inY, ok, err = AliasSource(p, "b.example.edu")
	if err != nil || !ok || !inY || a != "ours" {
		t.Errorf("Labmill host: %q %v %v %v", a, inY, ok, err)
	}
	if _, _, ok, err := AliasSource(p, "c.example.edu"); err != nil || ok {
		t.Errorf("unknown host: %v %v", ok, err)
	}

	// An alias matching in both files is reported as Labmill's (checked first).
	writeFile(t, p.LabmillConfig(), "Host ours\n    HostName a.example.edu\n")
	if a, inY, ok, _ := AliasSource(p, "a.example.edu"); !ok || !inY || a != "ours" {
		t.Errorf("both: %q %v %v", a, inY, ok)
	}

	// An alias reached through an Include from the user's config is the user's.
	p2 := tempPaths(t)
	writeFile(t, p2.UserConfig(), "Include extra\n")
	writeFile(t, p2.SSHDir+"/extra", "Host viaextra\n    HostName e.example.edu\n")
	if a, inY, ok, err := AliasSource(p2, "e.example.edu"); err != nil || !ok || inY || a != "viaextra" {
		t.Errorf("include: %q %v %v %v", a, inY, ok, err)
	}
}
