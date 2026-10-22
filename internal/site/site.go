// Package site loads the profile of the institution Labmill is used at: its
// login and transfer hosts, broker, two-factor help and first-run cluster
// templates. The profiles live in sites/<domain>/
// (embedded through package sites) as a Site.yaml next to a site skill
// (SKILL.md and References/*.md) that holds the institution's specifics for
// Claude. Everything specific to one cluster sits in a cluster folder,
// sites/<domain>/<cluster>/: a Cluster.yaml (the clusters.yaml template entry
// plus cluster hints such as the Slurm scratch facts) and the cluster's own
// References/*.md, which the site skill serves as "<cluster>/<file>.md". The
// user picks the site at first run; clusters.yaml records it as
// `site: <domain>`, and a config without it means the profile marked
// `default: true` in its Site.yaml (Generic when none is).
//
// Private profiles may be absent from a build: the repository carries them
// encrypted (docs/dev/sites-encryption.md) and only sites/generic is always
// there. Everything here copes with that: Default falls back to Generic.
package site

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing/fstest"
	"unicode"

	"go.yaml.in/yaml/v3"

	"labmill-client/internal/config"
	"labmill-client/internal/rules"
	"labmill-client/internal/skill"
	"labmill-client/internal/sshconfig"
	"labmill-client/sites"
)

// GenericDomain names the profile for "my institution isn't listed"
// (sites/generic): no hosts, no broker, and a site skill (site-generic) that
// explains how to find site facts without a profile. First-run setup then
// asks for the login host itself.
const GenericDomain = "generic"

// Host roles in Site.yaml.
const (
	RoleLogin    = "login"    // SSH login/submit host (Slurm commands)
	RoleTransfer = "transfer" // dedicated file-transfer host (SFTP)
	RoleJump     = "jump"     // bastion that other hosts ProxyJump through
)

// Host sign-in methods in Site.yaml (hosts[].auth).
const (
	AuthPassword = "password" // keyboard-interactive password, then 2FA (default)
	AuthKey      = "key"      // SSH public keys only; the server offers no password
)

// KeyUpload is where and how the user registers an SSH public key with the
// site (Site.yaml key_upload), shown by the key setup of a key-only host.
type KeyUpload struct {
	// URL is the https page where the key is uploaded ("" if unknown).
	URL string
	// Instruction is one line of what to do there ("" if unknown).
	Instruction string
}

// maxKeyInstructionRunes caps key_upload.instruction.
const maxKeyInstructionRunes = 300

// Site is one institution's profile.
type Site struct {
	// Domain is the institution's internet domain and the sites/ directory
	// name, e.g. "example.edu"; GenericDomain for the generic profile.
	Domain string
	// IsDefault marks the profile used when clusters.yaml names no site and
	// preselected at first run (Site.yaml `default: true`). At most one
	// embedded profile may set it; the generic profile never does.
	IsDefault bool
	// Name is the institution's name, shown in the first-run choice.
	Name string
	// Skill is the site skill's name ("site-" + Domain with dots as dashes;
	// "site-generic" for the generic profile), or "" when the build has no
	// skill files for it.
	Skill string
	// DocsURL is the institution's main HPC documentation page ("" if
	// unknown).
	DocsURL string
	// BrokerURL is the Labmill broker endpoint ("" when the site has none). The
	// LABMILL_BROKER_URL environment variable overrides it (Current, ForConfig).
	BrokerURL string
	// BrokerIssuers are the token issuers (Site.yaml `broker.issuers`) whose
	// users get this profile from the broker (ForIssuer). Each is an
	// absolute https URL without query or fragment, normalized without a
	// trailing slash; no issuer is claimed by two profiles.
	BrokerIssuers []string
	// TwoFactorHelp is shown when 2FA doesn't complete (not enrolled, no push).
	TwoFactorHelp string
	// TwoFactorURL is the 2FA enrollment page ("" if unknown).
	TwoFactorURL string
	// TwoFactorName is the 2FA product's name for user-facing text (e.g.
	// "Duo"); "" means generic wording.
	TwoFactorName string
	// NoS3Credentials is set by Site.yaml `s3_credentials_on_cluster: false`:
	// Labmill does not copy an AWS profile to the login node (M66.1).
	NoS3Credentials bool
	// Hosts are the SSH host templates the first-run setup writes to
	// ~/.ssh/config.d/labmill (User is filled in with the user's username).
	// ProxyJump, when set, names another template's Alias.
	Hosts []sshconfig.Host
	// Roles maps each Hosts alias to its role (RoleLogin, RoleTransfer,
	// RoleJump).
	Roles map[string]string
	// Auth maps each Hosts alias to how it signs in: AuthPassword (the
	// default: keyboard-interactive password plus 2FA) or AuthKey (SSH keys
	// only; Labmill guides the key setup, see KeyUpload).
	Auth map[string]string
	// KeyUpload says where the user registers a new SSH public key for the
	// site's AuthKey hosts (Site.yaml key_upload); zero when the profile
	// doesn't say.
	KeyUpload KeyUpload
	// Clusters are the cluster templates the first-run setup writes to
	// clusters.yaml; SSHHost and FilesHost name Hosts aliases. Site.yaml's
	// own clusters (older profiles) come first, then the templates of the
	// cluster folders, in folder name order.
	Clusters []config.Cluster
	// Folders are the site's cluster folders (sites/<domain>/<cluster>/), in
	// the order their templates appear in Clusters, folders without a
	// template last.
	Folders []ClusterFolder
	// ForbiddenTerms are extra site terms (project ids, bucket and asset
	// names, short host names, version strings) that must never appear in
	// the public generic skills: Site.yaml's forbidden_terms followed by
	// every Cluster.yaml's. The leak test in this package adds them to the
	// terms it derives from the profile (domain, hosts, aliases, cluster
	// names and folders, URL hosts). Labmill itself doesn't use them.
	ForbiddenTerms []string

	// skillFS holds the site skill: SKILL.md, References/*.md and
	// <cluster>/References/*.md at its root.
	skillFS fs.FS
	// rawYAML is Site.yaml's bytes (RawYAML).
	rawYAML []byte
}

// ClusterFolder is one cluster folder of a site profile,
// sites/<domain>/<Dir>/, as described by its Cluster.yaml.
type ClusterFolder struct {
	// Dir is the folder name (lowercase letters, digits and '-'; see
	// skill.SubfolderName), e.g. "bigiron". It need not equal the
	// template's cluster name.
	Dir string
	// Template is the clusters.yaml template entry, or nil when the folder
	// has no Cluster.yaml or its Cluster.yaml declares no cluster (only
	// comments, e.g. while the login host is unconfirmed).
	Template *config.Cluster
	// ForbiddenTerms are this cluster's extra leak-test terms (also merged
	// into Site.ForbiddenTerms).
	ForbiddenTerms []string
	// Slurm are the cluster's placement facts (Cluster.yaml preemptible:,
	// local_scratch:, global_scratch:; M61); zero when the profile says
	// nothing. Slurm clusters only.
	Slurm SlurmHints
	// Order is Cluster.yaml's order: (default 0). Among the folders with a
	// template, lower values come first in
	// Site.Clusters and so in a new clusters.yaml; equal values go by
	// folder name.
	Order int
	// rawYAML is Cluster.yaml's bytes (RawYAML).
	rawYAML []byte
}

// SlurmHintsFor returns the preemptible and scratch facts of the cluster
// folder whose template is named name; zero when the cluster has no folder
// or the folder sets none.
func (s Site) SlurmHintsFor(name string) SlurmHints {
	if f, ok := s.Cluster(name); ok {
		return f.Slurm
	}
	return SlurmHints{}
}

// Cluster returns the cluster folder whose template is named name (a
// clusters.yaml cluster name, e.g. "bigcluster"), for its hints.
func (s Site) Cluster(name string) (ClusterFolder, bool) {
	for _, f := range s.Folders {
		if f.Template != nil && f.Template.Name == name {
			return f, true
		}
	}
	return ClusterFolder{}, false
}

// SlurmHints are a Slurm cluster's preemptible and scratch facts (defined in
// package rules).
type SlurmHints = rules.SlurmHints

// LoginHost returns the HostName of the first host with RoleLogin, or "".
func (s Site) LoginHost() string {
	for _, h := range s.Hosts {
		if s.Roles[h.Alias] == RoleLogin {
			return h.HostName
		}
	}
	return ""
}

// KeyOnly reports whether the host named alias (a Hosts alias) accepts only
// SSH keys (auth: key).
func (s Site) KeyOnly(alias string) bool { return s.Auth[alias] == AuthKey }

// KeyOnlyHostNames returns the host names of the Hosts that accept only SSH
// keys, in profile order.
func (s Site) KeyOnlyHostNames() []string {
	var out []string
	for _, h := range s.Hosts {
		if s.KeyOnly(h.Alias) {
			out = append(out, h.HostName)
		}
	}
	return out
}

// file is Site.yaml as written.
type file struct {
	Domain    string `yaml:"domain"`
	Default   bool   `yaml:"default"`
	Name      string `yaml:"name"`
	Skill     string `yaml:"skill"`
	DocsURL   string `yaml:"docs_url"`
	BrokerURL string `yaml:"broker_url"`
	Broker    struct {
		Issuers []string `yaml:"issuers"`
	} `yaml:"broker"`
	TwoFactor struct {
		Help string `yaml:"help"`
		URL  string `yaml:"url"`
		Name string `yaml:"name"`
	} `yaml:"two_factor"`
	S3Creds *bool `yaml:"s3_credentials_on_cluster"`
	Hosts   []struct {
		Alias     string `yaml:"alias"`
		HostName  string `yaml:"hostname"`
		Role      string `yaml:"role"`
		Port      int    `yaml:"port"`
		ProxyJump string `yaml:"proxy_jump"`
		Auth      string `yaml:"auth"`
	} `yaml:"hosts"`
	KeyUpload struct {
		URL         string `yaml:"url"`
		Instruction string `yaml:"instruction"`
	} `yaml:"key_upload"`
	Clusters       []config.Cluster `yaml:"clusters"`
	ForbiddenTerms []string         `yaml:"forbidden_terms"`
}

// clusterFile is Cluster.yaml as written: a clusters.yaml entry (same keys)
// plus the cluster's hints and leak-test terms.
type clusterFile struct {
	config.Cluster `yaml:",inline"`
	Preemptible    *rules.Preemptible   `yaml:"preemptible"`
	LocalScratch   *rules.LocalScratch  `yaml:"local_scratch"`
	GlobalScratch  *rules.GlobalScratch `yaml:"global_scratch"`
	SoftwareDir    string               `yaml:"software_dir"`
	ForbiddenTerms []string             `yaml:"forbidden_terms"`
	Order          int                  `yaml:"order"`
}

var domainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// ValidDomain reports whether d looks like a lowercase internet domain with
// at least two labels (e.g. "example.edu").
func ValidDomain(d string) bool { return len(d) <= 253 && domainRE.MatchString(d) }

// SkillName returns the site skill name for a domain: "site-" + domain with
// dots replaced by dashes ("example.edu" → "site-example-edu").
func SkillName(domain string) string { return "site-" + strings.ReplaceAll(domain, ".", "-") }

// Parse decodes and validates one Site.yaml. Unknown keys are an error (so
// typos surface). It checks: domain is a ValidDomain (or GenericDomain); name is set; skill
// equals SkillName(domain); docs_url, broker_url and two_factor.url are empty
// or absolute https URLs; every host has a valid alias (unique), a hostname
// and a known role, a port in 0..65535 and a proxy_jump (if any) naming
// another host; auth is empty, "password" or "key"; key_upload.url is empty
// or an absolute https URL and key_upload.instruction one line of at most
// 300 characters; and the cluster templates pass config validation with
// ssh_host and files_host naming hosts; forbidden_terms entries are at least
// 3 characters. Empty values are fine: a profile may know only its name.
func Parse(data []byte) (Site, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f file
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return Site{}, fmt.Errorf("parse Site.yaml: %w", err)
	}
	if err := onlyDocument(dec); err != nil {
		return Site{}, fmt.Errorf("parse Site.yaml: %w", err)
	}
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !ValidDomain(f.Domain) && f.Domain != GenericDomain {
		bad("domain %q is not a lowercase internet domain", f.Domain)
	}
	if strings.TrimSpace(f.Name) == "" {
		bad("name is empty")
	}
	if f.Default && f.Domain == GenericDomain {
		bad("the generic profile can't be the default")
	}
	if f.Skill != SkillName(f.Domain) {
		bad("skill %q must be %q", f.Skill, SkillName(f.Domain))
	} else if !skill.ValidName(f.Skill) {
		bad("skill %q is not a valid skill name", f.Skill)
	}
	if n := f.TwoFactor.Name; n != "" {
		if len([]rune(n)) > 40 || strings.TrimSpace(n) != n {
			bad("two_factor.name %q must be plain text of at most 40 characters without surrounding spaces", n)
		} else if strings.IndexFunc(n, unicode.IsControl) >= 0 {
			bad("two_factor.name must not contain control characters")
		}
	}
	for _, u := range []struct{ key, v string }{
		{"docs_url", f.DocsURL}, {"broker_url", f.BrokerURL}, {"two_factor.url", f.TwoFactor.URL},
	} {
		if u.v == "" {
			continue
		}
		if p, err := url.Parse(u.v); err != nil || p.Scheme != "https" || p.Host == "" {
			bad("%s %q must be an absolute https URL", u.key, u.v)
		}
	}
	issuers, ierr := normalizeIssuers(f.Broker.Issuers)
	if ierr != nil {
		bad("broker.issuers: %v", ierr)
	}
	s := Site{
		BrokerIssuers:   issuers,
		Domain:          f.Domain,
		IsDefault:       f.Default,
		Name:            strings.TrimSpace(f.Name),
		Skill:           f.Skill,
		DocsURL:         f.DocsURL,
		BrokerURL:       f.BrokerURL,
		TwoFactorHelp:   strings.TrimSpace(f.TwoFactor.Help),
		TwoFactorURL:    f.TwoFactor.URL,
		TwoFactorName:   f.TwoFactor.Name,
		NoS3Credentials: f.S3Creds != nil && !*f.S3Creds,
		Roles:           map[string]string{},
		Auth:            map[string]string{},
		Clusters:        f.Clusters,
	}
	s.KeyUpload = KeyUpload{URL: f.KeyUpload.URL, Instruction: strings.TrimSpace(f.KeyUpload.Instruction)}
	if u := f.KeyUpload.URL; u != "" {
		if p, err := url.Parse(u); err != nil || p.Scheme != "https" || p.Host == "" {
			bad("key_upload.url %q must be an absolute https URL", u)
		}
	}
	if ins := s.KeyUpload.Instruction; len([]rune(ins)) > maxKeyInstructionRunes || strings.IndexFunc(ins, unicode.IsControl) >= 0 {
		bad("key_upload.instruction must be one line of at most %d characters", maxKeyInstructionRunes)
	}
	for i, h := range f.Hosts {
		who := fmt.Sprintf("host %d", i+1)
		if h.Alias != "" {
			who = fmt.Sprintf("host %q", h.Alias)
		}
		if err := sshconfig.ValidAlias(h.Alias); err != nil {
			bad("%s: %v", who, err)
		} else if _, dup := s.Roles[h.Alias]; dup {
			bad("%s: duplicate alias", who)
		}
		if h.HostName == "" {
			bad("%s: hostname is empty", who)
		}
		switch h.Role {
		case RoleLogin, RoleTransfer, RoleJump:
		default:
			bad("%s: unknown role %q (want %s, %s or %s)", who, h.Role, RoleLogin, RoleTransfer, RoleJump)
		}
		hh := sshconfig.Host{Alias: h.Alias, HostName: h.HostName, Port: h.Port, ProxyJump: h.ProxyJump}
		if err := hh.Validate(); err != nil {
			bad("%s: %v", who, err)
		}
		switch h.Auth {
		case "", AuthPassword:
			s.Auth[h.Alias] = AuthPassword
		case AuthKey:
			s.Auth[h.Alias] = AuthKey
		default:
			bad("%s: unknown auth %q (want %s or %s)", who, h.Auth, AuthPassword, AuthKey)
		}
		s.Roles[h.Alias] = h.Role
		s.Hosts = append(s.Hosts, hh)
	}
	for _, h := range s.Hosts {
		if h.ProxyJump == "" {
			continue
		}
		if _, ok := s.Roles[h.ProxyJump]; !ok || h.ProxyJump == h.Alias {
			bad("host %q: proxy_jump %q must name another host in this file", h.Alias, h.ProxyJump)
		}
	}
	s.ForbiddenTerms = parseTerms(f.ForbiddenTerms, bad)
	if err := (&config.Config{Clusters: s.Clusters}).Validate(); err != nil {
		bad("clusters: %v", err)
	}
	checkHostRefs(s.Clusters, s.Roles, "in this file", bad)
	if err := errors.Join(errs...); err != nil {
		return Site{}, fmt.Errorf("Site.yaml (%s): %w", f.Domain, err)
	}
	s.rawYAML = append([]byte(nil), data...)
	return s, nil
}

// parseTerms checks forbidden_terms: each at least 3 characters (trimmed).
func parseTerms(terms []string, bad func(string, ...any)) []string {
	var out []string
	for i, term := range terms {
		term = strings.TrimSpace(term)
		if len(term) < 3 {
			bad("forbidden_terms %d: %q is shorter than 3 characters", i+1, term)
			continue
		}
		out = append(out, term)
	}
	return out
}

// checkHostRefs reports every ssh_host or files_host of clusters that isn't
// a key of roles (the hosts in Site.yaml); where says where those hosts are.
func checkHostRefs(clusters []config.Cluster, roles map[string]string, where string, bad func(string, ...any)) {
	for _, c := range clusters {
		for _, ref := range []struct{ key, v string }{{"ssh_host", c.SSHHost}, {"files_host", c.FilesHost}} {
			if ref.v == "" {
				continue
			}
			if _, ok := roles[ref.v]; !ok {
				bad("cluster %q: %s %q is not one of the hosts %s", c.Name, ref.key, ref.v, where)
			}
		}
	}
}

// onlyDocument reports an error when dec holds another YAML document after
// the one already decoded: a second document ("---") would otherwise be
// ignored silently.
func onlyDocument(dec *yaml.Decoder) error {
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("only one YAML document is allowed (remove the extra \"---\" section)")
	}
	return nil
}

// ParseCluster decodes and validates the Cluster.yaml of the cluster folder
// dir. Unknown keys are an error. dir must pass skill.SubfolderName. A
// document without any cluster key (empty, or only comments) declares no
// template; otherwise name and type are required and the entry must pass
// the same validation as a clusters.yaml entry; forbidden_terms are checked
// as in Parse. preemptible, local_scratch,
// global_scratch and software_dir (M61, M67.1; rules.SlurmHints, validated by SlurmHints.Validate) are
// only for a Slurm template and become Slurm. Host references (ssh_host, files_host) are checked by
// Load against Site.yaml's hosts, not here.
func ParseCluster(dir string, data []byte) (ClusterFolder, error) {
	if !skill.SubfolderName(dir) {
		return ClusterFolder{}, fmt.Errorf("cluster folder %q: the name must be lowercase letters, digits and '-' (at most 63, not References, Evals, Diagrams or Tools)", dir)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f clusterFile
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return ClusterFolder{}, fmt.Errorf("parse %s/Cluster.yaml: %w", dir, err)
	}
	if err := onlyDocument(dec); err != nil {
		return ClusterFolder{}, fmt.Errorf("parse %s/Cluster.yaml: %w", dir, err)
	}
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	cf := ClusterFolder{Dir: dir, Order: f.Order}
	c := f.Cluster
	if c != (config.Cluster{}) {
		if err := (&config.Config{Clusters: []config.Cluster{c}}).Validate(); err != nil {
			bad("%v", err)
		}
		cf.Template = &c
	}
	cf.Slurm = SlurmHints{Preemptible: f.Preemptible, LocalScratch: f.LocalScratch, GlobalScratch: f.GlobalScratch, SoftwareDir: f.SoftwareDir}
	if !cf.Slurm.Zero() {
		if cf.Template == nil || cf.Template.Type != config.TypeSlurm {
			bad("preemptible, local_scratch, global_scratch and software_dir are only for a slurm cluster")
		} else if err := cf.Slurm.Validate(); err != nil {
			bad("%v", err)
		}
	}
	cf.ForbiddenTerms = parseTerms(f.ForbiddenTerms, bad)
	if err := errors.Join(errs...); err != nil {
		return ClusterFolder{}, fmt.Errorf("%s/Cluster.yaml: %w", dir, err)
	}
	cf.rawYAML = append([]byte(nil), data...)
	return cf, nil
}

// reservedFolders are the folders of a site folder that aren't cluster
// folders (cmd/labmill-sites has the same list).
var reservedFolders = map[string]bool{"References": true, "Evals": true, "Diagrams": true, "Tools": true}

// loadFolders reads the cluster folders of <domain>/ in fsys: every
// directory except the reserved ones and those starting with '_' or '.'
// (Labmill ignores them, e.g. generic/_cluster-template). Each must have a
// valid folder name; its Cluster.yaml is optional. The result is ordered:
// templates (by Cluster.yaml's order:, lower first, then by folder name),
// then the folders without a template (by folder name).
func loadFolders(fsys fs.FS, domain string) ([]ClusterFolder, error) {
	entries, err := fs.ReadDir(fsys, domain)
	if err != nil {
		return nil, err
	}
	var out []ClusterFolder
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || reservedFolders[name] || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		if !skill.SubfolderName(name) {
			return nil, fmt.Errorf("cluster folder %q: the name must be lowercase letters, digits and '-' (at most 63 characters)", name)
		}
		data, err := fs.ReadFile(fsys, path.Join(domain, name, "Cluster.yaml"))
		if errors.Is(err, fs.ErrNotExist) {
			out = append(out, ClusterFolder{Dir: name})
			continue
		}
		if err != nil {
			return nil, err
		}
		cf, err := ParseCluster(name, data)
		if err != nil {
			return nil, err
		}
		out = append(out, cf)
	}
	rank := func(f ClusterFolder) int {
		if f.Template == nil {
			return 2
		}
		return 0
	}
	sort.SliceStable(out, func(i, j int) bool {
		if rank(out[i]) != rank(out[j]) {
			return rank(out[i]) < rank(out[j])
		}
		if rank(out[i]) < 2 && out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Dir < out[j].Dir
	})
	return out, nil
}

// addFolders merges the cluster folders into s: their templates are
// appended to s.Clusters (names unique across the site, host references
// naming Site.yaml's hosts) and their forbidden_terms to s.ForbiddenTerms.
func addFolders(s *Site, folders []ClusterFolder) error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	s.Clusters = append([]config.Cluster(nil), s.Clusters...)
	var templates []config.Cluster
	for _, f := range folders {
		s.ForbiddenTerms = append(s.ForbiddenTerms, f.ForbiddenTerms...)
		if f.Template == nil {
			continue
		}
		templates = append(templates, *f.Template)
	}
	s.Clusters = append(s.Clusters, templates...)
	if err := (&config.Config{Clusters: s.Clusters}).Validate(); err != nil {
		bad("clusters: %v", err)
	}
	checkHostRefs(templates, s.Roles, "in Site.yaml", bad)
	s.Folders = folders
	return errors.Join(errs...)
}

// load reads <domain>/Site.yaml and the cluster folders' Cluster.yaml from
// fsys, checks that the domain matches the directory and that the directory
// holds a SKILL.md, and attaches the skill files (SKILL.md, references/ and
// <cluster>/References/; Site.yaml, Cluster.yaml, QUESTIONS.md, evals and
// diagrams stay out) to the Site.
func load(fsys fs.FS, domain string) (Site, error) {
	data, err := fs.ReadFile(fsys, path.Join(domain, "Site.yaml"))
	if err != nil {
		return Site{}, fmt.Errorf("site %q: %w", domain, err)
	}
	s, err := Parse(data)
	if err != nil {
		return Site{}, err
	}
	if s.Domain != domain {
		return Site{}, fmt.Errorf("site %q: Site.yaml says domain %q", domain, s.Domain)
	}
	folders, err := loadFolders(fsys, domain)
	if err != nil {
		return Site{}, fmt.Errorf("site %q: %w", domain, err)
	}
	if err := addFolders(&s, folders); err != nil {
		return Site{}, fmt.Errorf("site %q: %w", domain, err)
	}
	if _, err := fs.Stat(fsys, path.Join(domain, "SKILL.md")); err != nil {
		return Site{}, fmt.Errorf("site %q: %w", domain, err)
	}
	sub, err := fs.Sub(fsys, domain)
	if err != nil {
		return Site{}, fmt.Errorf("site %q: %w", domain, err)
	}
	s.skillFS = sub
	return s, nil
}

// Load reads one profile directory, <domain>/ in fsys (Site.yaml, SKILL.md,
// references/ and the cluster folders), with the same checks Get applies to
// the embedded profiles.
// Get uses the embedded sites; tests and tools use Load for fixture profiles.
func Load(fsys fs.FS, domain string) (Site, error) { return load(fsys, domain) }

// Generic returns the profile for an institution that isn't listed
// (sites/generic): no hosts, clusters or broker. If the build lacks sites/generic, the same profile without a skill is
// returned.
func Generic() Site {
	if s, err := load(sites.FS, GenericDomain); err == nil {
		return s
	}
	return Site{
		Domain: GenericDomain,
		Name:   "My institution isn't listed",
		Roles:  map[string]string{},
	}
}

// Get returns the embedded profile for domain, or Generic for GenericDomain.
// A domain whose profile isn't in this build is an error. No environment
// overrides are applied.
func Get(domain string) (Site, error) {
	if domain == GenericDomain {
		return Generic(), nil
	}
	if !ValidDomain(domain) {
		return Site{}, fmt.Errorf("%q is not a known site", domain)
	}
	if _, err := fs.Stat(sites.FS, path.Join(domain, "Site.yaml")); err != nil {
		return Site{}, fmt.Errorf("this Labmill has no profile for %q", domain)
	}
	return load(sites.FS, domain)
}

// List returns every embedded institution profile, the one marked default
// first, the rest sorted by Name. A profile that fails to load is skipped
// (the tests make sure none does). Generic is not included; Choices adds it.
// If two profiles are marked default (see listChecked) none is.
func List() []Site {
	out, _ := listChecked(sites.FS)
	return out
}

// Check returns the configuration error that List swallows (a profile that
// fails to load, two profiles marked default, one broker issuer claimed
// twice), nil when the embedded
// profiles are fine.
func Check() error {
	_, err := listChecked(sites.FS)
	return err
}

func listFS(fsys fs.FS) []Site {
	out, _ := listChecked(fsys)
	return out
}

// listChecked is listFS that also reports a configuration error: more than
// one profile marked `default: true`, or one broker issuer claimed by two
// profiles. The profiles are then still returned,
// sorted by Name with the default flag cleared, so a broken build keeps
// working with the generic default.
func listChecked(fsys fs.FS) ([]Site, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, nil
	}
	var out []Site
	var derr error
	for _, e := range entries {
		if !e.IsDir() || e.Name() == GenericDomain {
			continue
		}
		s, err := load(fsys, e.Name())
		if err != nil {
			derr = errors.Join(derr, fmt.Errorf("profile %s does not load: %w", e.Name(), err))
			continue
		}
		out = append(out, s)
	}
	var defaults []string
	for _, s := range out {
		if s.IsDefault {
			defaults = append(defaults, s.Domain)
		}
	}
	claimed := map[string]string{}
	for _, s := range out {
		for _, iss := range s.BrokerIssuers {
			if other, ok := claimed[iss]; ok && other != s.Domain {
				derr = errors.Join(derr, fmt.Errorf("broker issuer %s is claimed by both %s and %s", iss, other, s.Domain))
				continue
			}
			claimed[iss] = s.Domain
		}
	}
	if len(defaults) > 1 {
		derr = errors.Join(derr, fmt.Errorf("sites %s are all marked default: true; only one may be", strings.Join(defaults, ", ")))
		for i := range out {
			out[i].IsDefault = false
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsDefault != out[j].IsDefault {
			return out[i].IsDefault
		}
		return out[i].Name < out[j].Name
	})
	return out, derr
}

// normalizeIssuer trims surrounding spaces and one trailing slash, so
// "https://idp.example.edu/realms/x/" and "https://idp.example.edu/realms/x"
// are the same issuer.
func normalizeIssuer(iss string) string {
	return strings.TrimSuffix(strings.TrimSpace(iss), "/")
}

// normalizeIssuers validates a Site.yaml issuer list: each an absolute https
// URL with a host and no query or fragment, no duplicates after
// normalization. It returns the normalized list.
func normalizeIssuers(in []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range in {
		iss := normalizeIssuer(raw)
		p, err := url.Parse(iss)
		if err != nil || p.Scheme != "https" || p.Host == "" || p.Opaque != "" || p.User != nil {
			return nil, fmt.Errorf("%q must be an absolute https URL", raw)
		}
		if p.RawQuery != "" || p.Fragment != "" || strings.ContainsAny(iss, "?#") {
			return nil, fmt.Errorf("%q must not have a query or fragment", raw)
		}
		if seen[iss] {
			return nil, fmt.Errorf("%q is listed twice", raw)
		}
		seen[iss] = true
		out = append(out, iss)
	}
	return out, nil
}

// ForIssuer returns the embedded profile whose broker.issuers lists iss (a
// trailing slash is ignored), or Generic and false when none does or the
// build wrongly lets two profiles claim it.
func ForIssuer(iss string) (Site, bool) { return forIssuer(List(), iss) }

// ForIssuerIn is ForIssuer over the given list instead of the embedded one.
func ForIssuerIn(list []Site, iss string) (Site, bool) { return forIssuer(list, iss) }

func forIssuer(list []Site, iss string) (Site, bool) {
	iss = normalizeIssuer(iss)
	if iss == "" {
		return Generic(), false
	}
	var found []Site
	for _, s := range list {
		for _, i := range s.BrokerIssuers {
			if i == iss {
				found = append(found, s)
			}
		}
	}
	if len(found) != 1 {
		return Generic(), false
	}
	return found[0], true
}

// Choices is List followed by Generic: the options of the first-run
// institution choice.
func Choices() []Site { return append(List(), Generic()) }

// Default returns the embedded profile marked `default: true`, or Generic
// when none is (or this build doesn't carry it, or two claim it).
func Default() Site { return defaultOf(List()) }

func defaultOf(list []Site) Site {
	for _, s := range list {
		if s.IsDefault {
			return s
		}
	}
	return Generic()
}

// applyEnv applies environment overrides: LABMILL_BROKER_URL replaces
// BrokerURL when set.
func applyEnv(s Site) Site {
	if u := strings.TrimSpace(os.Getenv("LABMILL_BROKER_URL")); u != "" {
		s.BrokerURL = u
	}
	return s
}

// ForConfig returns the site cfg names (cfg.Site; "" or a nil cfg means
// Default().Domain), with environment overrides applied. A site this build
// doesn't know returns Generic (with overrides) and an error the caller
// shows as a warning: the user keeps a working Labmill.
func ForConfig(cfg *config.Config) (Site, error) {
	domain, named := Default().Domain, false
	if cfg != nil && cfg.Site != "" {
		domain, named = cfg.Site, true
	}
	s, err := Get(domain)
	if err != nil {
		if named {
			return applyEnv(Generic()), fmt.Errorf("clusters.yaml names the site %q, but %v; using the generic profile", domain, err)
		}
		return applyEnv(Generic()), fmt.Errorf("clusters.yaml names no site and %v (the default); using the generic profile", err)
	}
	return applyEnv(s), nil
}

// Current returns the site named in the user's clusters.yaml (the default
// site when there is no config yet, it can't be read, or it names no site), with
// environment overrides applied. Front ends use the Site from launch.Build;
// Current is for subcommands that don't build a runtime (ssh-exec).
func Current() Site {
	var cfg *config.Config
	if p, err := config.DefaultPath(); err == nil {
		cfg, _ = config.Load(p)
	}
	s, _ := ForConfig(cfg)
	return s
}

// Others returns the embedded institution profiles other than home (by
// Domain) and Generic, sorted by Name: the institutions a session knows
// besides the user's own. Their cluster names are in ClusterNames.
func Others(home string) []Site { return othersOf(List(), home) }

func othersOf(list []Site, home string) []Site {
	var out []Site
	for _, s := range list {
		if s.Domain != home && s.Domain != GenericDomain {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ClusterNames returns the cluster names of the site's templates (Clusters),
// in order.
func (s Site) ClusterNames() []string {
	var out []string
	for _, c := range s.Clusters {
		out = append(out, c.Name)
	}
	return out
}

// SkillsFS returns generic (the generic skills, one directory per skill)
// with this site's skill added as the directory s.Skill, so skill.Load sees
// the site skill under its own name. For a site without a skill it returns
// generic unchanged. It is an error if generic already has a directory
// named s.Skill.
func (s Site) SkillsFS(generic fs.FS) (fs.FS, error) { return SkillsFSAll(generic, s, nil) }

// SkillsFSAll is SkillsFS for a session that knows several institutions:
// generic with the skills of home and of every site in others added, each as
// the directory of its Skill (home first; a site without a skill, or listed
// twice, is skipped). It is an error if generic already has a directory
// named like a site skill.
func SkillsFSAll(generic fs.FS, home Site, others []Site) (fs.FS, error) {
	fsys, _, err := skillsFS(generic, home, others, true)
	return fsys, err
}

// SkillsFSSession is SkillsFSAll for a session: only the home site's skill
// is essential. A clash or read error in the skill of another institution
// leaves that skill out and comes back as a warning line instead of an
// error, so one broken profile of an institution the user doesn't use can't
// stop Labmill.
func SkillsFSSession(generic fs.FS, home Site, others []Site) (fs.FS, []string, error) {
	return skillsFS(generic, home, others, false)
}

func skillsFS(generic fs.FS, home Site, others []Site, strict bool) (fs.FS, []string, error) {
	var warnings []string
	all := append([]Site{home}, others...)
	var withSkill []Site
	seen := map[string]bool{}
	for i, s := range all {
		if s.Skill == "" || s.skillFS == nil || seen[s.Skill] {
			continue
		}
		seen[s.Skill] = true
		if _, err := fs.Stat(generic, s.Skill); err == nil {
			err := fmt.Errorf("site skill %q clashes with a generic skill of the same name", s.Skill)
			if strict || i == 0 {
				return nil, nil, err
			}
			warnings = append(warnings, "skipped the skill of "+s.Name+": "+err.Error())
			continue
		}
		withSkill = append(withSkill, s)
	}
	if len(withSkill) == 0 {
		return generic, warnings, nil
	}
	out := fstest.MapFS{}
	copyTree := func(src fs.FS, prefix string, skip func(string) bool) error {
		return fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || skip(p) {
				return nil
			}
			data, err := fs.ReadFile(src, p)
			if err != nil {
				return err
			}
			out[path.Join(prefix, p)] = &fstest.MapFile{Data: data, Mode: 0o444}
			return nil
		})
	}
	if err := copyTree(generic, ".", func(string) bool { return false }); err != nil {
		return nil, nil, fmt.Errorf("read skills: %w", err)
	}
	// Keep SKILL.md, References/*.md and <cluster>/References/*.md.
	onlySkill := func(p string) bool {
		if p == "SKILL.md" {
			return false
		}
		if !strings.HasSuffix(p, ".md") {
			return true
		}
		parts := strings.Split(p, "/")
		switch {
		case len(parts) == 2:
			return parts[0] != "References"
		case len(parts) == 3:
			return parts[1] != "References" || !skill.SubfolderName(parts[0])
		}
		return true
	}
	isHome := func(s Site) bool { return home.Skill != "" && s.Skill == home.Skill }
	for _, s := range withSkill {
		if err := copyTree(s.skillFS, s.Skill, onlySkill); err != nil {
			if strict || isHome(s) {
				return nil, nil, fmt.Errorf("read site skill %q: %w", s.Skill, err)
			}
			warnings = append(warnings, fmt.Sprintf("skipped the skill of %s: read site skill %q: %v", s.Name, s.Skill, err))
			// Drop what was copied of it.
			for p := range out {
				if strings.HasPrefix(p, s.Skill+"/") {
					delete(out, p)
				}
			}
		}
	}
	return out, warnings, nil
}
