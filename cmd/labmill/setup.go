package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"charm.land/huh/v2"

	"labmill-client/internal/launch"
	"labmill-client/internal/site"
)

// askSetup shows the first-run form (a Huh form run standalone, before the
// main TUI): the institution (site.Choices, s preselected, plus "my
// institution isn't listed"), the username (required; [a-z0-9._-]{1,32}),
// then, only when the chosen site has no login host (launch.NeedsHosts), the
// login host (required: it names the home institution, M55), and a confirm. When clusters.yaml had no home
// institution, launch.SetupNote is shown first. It returns an error if the
// user cancels.
var askSetup launch.SetupFunc = func(ctx context.Context, s site.Site) (launch.SetupAnswers, error) {
	a := launch.SetupAnswers{Site: s.Domain}
	choices := site.Choices()
	bySite := map[string]site.Site{}
	var opts []huh.Option[string]
	for _, c := range choices {
		bySite[c.Domain] = c
		label := c.Name
		if c.Domain != site.GenericDomain {
			label = fmt.Sprintf("%s (%s)", c.Name, c.Domain)
		}
		opts = append(opts, huh.NewOption(label, c.Domain))
	}
	only := launch.SetupOnlyInstitution(ctx) // clusters exist: only the institution is saved
	needsHosts := func() bool { return launch.NeedsHosts(bySite[a.Site]) }
	ok := true
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewNote().
				Title("Welcome to labmill").
				Description(welcomeText(launch.SetupNote(ctx))),
			huh.NewSelect[string]().
				Title("Your institution").
				Description("Pick your home institution, where your cluster account and most of your data are.").
				Options(opts...).
				Value(&a.Site),
		),
		huh.NewGroup(
			huh.NewInput().
				Title("Your account name").
				DescriptionFunc(func() string {
					return "The user name you use to sign in to the cluster at " + bySite[a.Site].Name + "."
				}, &a.Site).
				Value(&a.Username).
				Validate(func(v string) error {
					if !launch.ValidUsername(strings.TrimSpace(v)) {
						return errors.New("use only lowercase letters, digits, dots, dashes and underscores (up to 32 characters)")
					}
					return nil
				}),
		).WithHideFunc(func() bool { return only }),
		huh.NewGroup(
			huh.NewInput().
				Title("Cluster login host").
				Description("The host name you would type after ssh, for example login.example.edu. It tells Labmill which institution is your home.").
				Value(&a.LoginHost).
				Validate(func(v string) error {
					if v = strings.TrimSpace(v); v != "" && !launch.ValidLoginHost(v) {
						return errors.New("enter just the host name, for example login.example.edu")
					}
					return hostsGiven(bySite[a.Site], v)
				}),
		).WithHideFunc(func() bool { return only || !needsHosts() }),
		huh.NewGroup(
			huh.NewConfirm().
				Title("Save these settings?").
				DescriptionFunc(func() string {
					if only {
						return "Labmill adds your home institution to your settings (clusters.yaml); your clusters stay as they are."
					}
					return "Labmill adds the cluster login hosts to your SSH settings (a separate file, ~/.ssh/config.d/labmill)."
				}, &a.Site).
				Affirmative("Yes").Negative("No").
				Value(&ok),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return launch.SetupAnswers{}, errors.New("setup canceled")
		}
		return launch.SetupAnswers{}, fmt.Errorf("setup: %w", err)
	}
	if !ok {
		return launch.SetupAnswers{}, errors.New("setup canceled")
	}
	a.Username = strings.TrimSpace(a.Username)
	a.LoginHost = strings.TrimSpace(a.LoginHost)
	if only {
		a.Username = ""
	}
	if only || !needsHosts() {
		a.LoginHost = ""
	}
	return a, nil
}

// welcomeText is the first form's text: the greeting, with the plain note
// (launch.SetupNote) when clusters.yaml named no home institution.
func welcomeText(note string) string {
	t := "Let's connect Labmill to your institution's clusters. Your password and two-factor sign-in are asked for when you first connect."
	if note != "" {
		t = note + "\n\n" + t
	}
	return t
}

// hostsGiven is the check of the host fields: a site without a login host
// needs one, since it names the home institution (M55).
func hostsGiven(s site.Site, loginHost string) error {
	if err := launch.HostsRequired(s, loginHost); err != nil {
		return errors.New("enter the login host of your institution")
	}
	return nil
}
