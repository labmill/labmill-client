package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/huh/v2"

	"labmill-client/internal/account"
	"labmill-client/internal/launch"
	"labmill-client/internal/mailreq"
	"labmill-client/internal/site"
)

// askSetup shows the first-run form (a Huh form run standalone, before the
// main TUI). The user signs in to the Labmill account first (the browser
// opens, owner 2026-11-12), since the institution list is for signed-in
// users only. Then: a search for the home institution and a pick from the
// account's answers (or "my institution isn't listed"), the username
// (required; [a-z0-9._-]{1,32}), the login host only when the chosen
// profile has none (launch.NeedsHosts; required then: it names the home
// institution, M55), and a confirm. When clusters.yaml had no home
// institution, launch.SetupNote is shown first. It returns an error if the
// user cancels.
var askSetup launch.SetupFunc = func(ctx context.Context, _ site.Site) (launch.SetupAnswers, error) {
	client, err := signedInClient(ctx)
	if err != nil {
		return launch.SetupAnswers{}, err
	}
	var a launch.SetupAnswers
	var query, pick string
	found := map[string]account.Institution{}
	only := launch.SetupOnlyInstitution(ctx) // clusters exist: only the institution is saved
	profile := func() site.Site {
		st, _ := site.Get(launch.SiteFor(found[pick].Domain))
		return st
	}
	needsHosts := func() bool { return launch.NeedsHosts(profile()) }
	ok := true
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewNote().
				Title("Welcome to labmill").
				Description(welcomeText(launch.SetupNote(ctx))),
			huh.NewInput().
				Title("Your home institution").
				Description("Type part of its name, for example oregon. Your home institution is where your cluster account is.").
				Value(&query).
				Validate(func(v string) error {
					if len([]rune(strings.TrimSpace(v))) < 2 {
						return errors.New("type at least 2 letters")
					}
					return nil
				}),
		),
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Pick your institution").
				OptionsFunc(func() []huh.Option[string] {
					sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
					defer cancel()
					list, _ := client.Institutions(sctx, strings.TrimSpace(query))
					var opts []huh.Option[string]
					for _, i := range list {
						found[i.ID] = i
						label := i.Name
						if where := strings.Trim(i.City+", "+i.Country, ", "); where != "" {
							label += " (" + where + ")"
						}
						opts = append(opts, huh.NewOption(label, i.ID))
					}
					return append(opts, huh.NewOption("My institution isn't listed", ""))
				}, &query).
				Height(12).
				Value(&pick),
		),
		huh.NewGroup(
			huh.NewInput().
				Title("Your account name").
				Description("The user name you use to sign in to your institution's cluster.").
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
					return hostsGiven(profile(), v)
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
				}, &pick).
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
	a.Site = profile().Domain
	if i, listed := found[pick]; listed && pick != "" {
		a.Home = &launch.Home{ID: i.ID, Name: i.Name, Domain: i.Domain, Country: i.Country}
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

// signedInClient returns the account client, after a browser sign-in when
// this computer is not connected yet.
func signedInClient(ctx context.Context) (*account.Client, error) {
	path, err := account.Path()
	if err != nil {
		return nil, err
	}
	l, ok, err := account.Load(path)
	if err != nil {
		return nil, err
	}
	if !ok {
		base, err := account.BaseURL()
		if err != nil {
			return nil, err
		}
		host, _ := os.Hostname()
		fmt.Fprintln(os.Stderr, "Welcome to labmill. First sign in to your Labmill account (Google, GitHub, LinkedIn or ORCID).")
		if _, err := account.BrowserLogin(ctx, path, base, host, func(u string) error {
			fmt.Fprintf(os.Stderr, "Your browser opens. If it does not, open this address:\n  %s\n", u)
			_ = mailreq.Open(u)
			return nil
		}, nil, time.Now()); err != nil {
			return nil, fmt.Errorf("sign in to Labmill first: %w", err)
		}
		if l, _, err = account.Load(path); err != nil {
			return nil, err
		}
	}
	return &account.Client{URL: l.URL, Token: l.Token}, nil
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
