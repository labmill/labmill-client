package launch

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"labmill-client/internal/site"
)

// Home is the home institution picked from the Labmill account's list at first run (owner, 2026-11-12). It is kept
// in home.json next to clusters.yaml; clusters.yaml keeps `site:`, the profile Labmill uses for it.
type Home struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Domain   string    `json:"domain,omitempty"`
	Country  string    `json:"country,omitempty"`
	ChosenAt time.Time `json:"chosen_at"`
}

// HomePath is home.json next to clusters.yaml.
func HomePath(cfgPath string) string { return filepath.Join(filepath.Dir(cfgPath), "home.json") }

// SaveHome writes home.json atomically.
func SaveHome(cfgPath string, h Home) error {
	b, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	p := HomePath(cfgPath)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// LoadHome reads home.json; ok is false when there is none.
func LoadHome(cfgPath string) (h Home, ok bool, err error) {
	b, err := os.ReadFile(HomePath(cfgPath))
	if errors.Is(err, fs.ErrNotExist) {
		return Home{}, false, nil
	}
	if err != nil {
		return Home{}, false, err
	}
	return h, true, json.Unmarshal(b, &h)
}

// SiteFor is the profile for an institution's web domain: its own when this build has one, else the generic profile.
func SiteFor(domain string) string {
	if domain != "" {
		if s, err := site.Get(domain); err == nil {
			return s.Domain
		}
	}
	return site.GenericDomain
}
