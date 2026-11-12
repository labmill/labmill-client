package launch

import (
	"context"
	"testing"

	"labmill-client/internal/site"
)

// A first run with an institution from the account's list saves it in home.json and uses the generic profile when
// this build has no profile for its domain.
func TestFirstRunSavesHome(t *testing.T) {
	cfgPath := buildEnv(t)
	if SiteFor("uoregon.edu") != site.GenericDomain || SiteFor("") != site.GenericDomain {
		t.Fatal("SiteFor: no profile must give the generic one")
	}
	setup := func(context.Context, site.Site) (SetupAnswers, error) {
		return SetupAnswers{Site: SiteFor("uoregon.edu"), Username: "dana", LoginHost: "talapas-login.uoregon.edu",
			Home: &Home{ID: "i1", Name: "University of Oregon", Domain: "uoregon.edu", Country: "US"}}, nil
	}
	rt, err := Build(context.Background(), &fakeUI{done: make(chan error, 1)}, setup)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, ok, err := LoadHome(cfgPath)
	if err != nil || !ok || h.ID != "i1" || h.Name != "University of Oregon" || h.ChosenAt.IsZero() {
		t.Fatalf("home.json: %+v %v %v", h, ok, err)
	}
}

// "My institution isn't listed" writes no home.json.
func TestFirstRunNotListedNoHome(t *testing.T) {
	cfgPath := buildEnv(t)
	setup := func(context.Context, site.Site) (SetupAnswers, error) {
		return SetupAnswers{Site: site.GenericDomain, Username: "dana", LoginHost: "hpc.example.edu"}, nil
	}
	rt, err := Build(context.Background(), &fakeUI{done: make(chan error, 1)}, setup)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if _, ok, err := LoadHome(cfgPath); ok || err != nil {
		t.Fatalf("home.json for not listed: %v %v", ok, err)
	}
}
