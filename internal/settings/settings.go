// Package settings is the logic behind the desktop app's Settings page and
// the terminal's /settings command (M29): it reads every setting Labmill keeps
// into one View (never a secret: no passwords, tokens, keys or the sites
// key), and saves the user's changes with the same rules that apply when
// the files are loaded.
//
// What it writes, and how:
//   - clusters.yaml (the institution and the clusters): edited in place on
//     the YAML node tree (config.SetSiteYAML, PutClusterYAML,
//     RemoveClusterYAML), so comments and key order survive; validated with
//     config.FieldErrors first (errors come back per field for inline
//     display); backed up once per session before the first write
//     (config.Backup, O_EXCL, never overwriting an older backup); written
//     atomically with mode 0600 (config.WriteFile).
//   - Labmill's own SSH file, ~/.ssh/config.d/labmill: one host block at a time
//     (sshconfig.PutLabmillHost, RemoveLabmillHost; atomic, 0600). The user's
//     own ~/.ssh/config is only read (sshconfig.ListHosts) and never
//     rewritten.
//
// Changes to these files take effect the next time Labmill starts (the
// running session loaded them at startup); View.Restart says so once a save
// happened. The desktop app's own settings (start in the tray, one-time
// notices) and the chosen folders are saved by their owners (gui, app).
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"labmill-client/internal/config"
	"labmill-client/internal/site"
	"labmill-client/internal/sshconfig"
	"labmill-client/internal/store"
)

// Paths are the files the settings live in.
type Paths struct {
	Config string          // clusters.yaml
	SSH    sshconfig.Paths // Labmill's file (config.d/labmill) and the user's config
	Prefs  string          // gui.json, the desktop app's own settings
	State  string          // labmill.db, the job database
	Log    string          // labmill.log (startup warnings)
}

// DefaultPaths returns the real locations: config.DefaultPath,
// sshconfig.DefaultPaths, gui.json next to clusters.yaml, store.DefaultPath
// and labmill.log next to the job database.
func DefaultPaths() (Paths, error) {
	cfg, err := config.DefaultPath()
	if err != nil {
		return Paths{}, err
	}
	ssh, err := sshconfig.DefaultPaths()
	if err != nil {
		return Paths{}, err
	}
	db, err := store.DefaultPath()
	if err != nil {
		return Paths{}, err
	}
	return Paths{
		Config: cfg,
		SSH:    ssh,
		Prefs:  filepath.Join(filepath.Dir(cfg), "gui.json"),
		State:  db,
		Log:    filepath.Join(filepath.Dir(db), "labmill.log"),
	}, nil
}

// Service reads and saves the settings. The zero value is not usable: set
// Paths (and GOOS, which picks the SSH block format; "" means runtime.GOOS).
// Its methods are safe for concurrent use.
type Service struct {
	Paths Paths
	GOOS  string
	Now   func() time.Time // nil means time.Now
	// Profile, if set, replaces the institution's profile that
	// View.MoreClusters is computed from (a test seam: the embedded
	// profiles may be absent).
	Profile func(cfg *config.Config) site.Site

	mu      sync.Mutex
	backup  string // the backup of clusters.yaml made this session
	changed bool   // a file was saved this session (restart to apply)
}

// Info is what the running app knows that the files don't.
type Info struct {
	Version   string
	BrokerURL string
	// LoggedIn lists the aliases the connection helper is logged in to;
	// LoginKnown is false when that is unknown (no helper reached).
	LoggedIn   []string
	LoginKnown bool
	// StartInTray and NoticesShown are the desktop app's own settings.
	StartInTray  bool
	NoticesShown bool
	// RunWithoutAsking is "Run commands on the cluster without asking"
	// (AutoRunRemote reads it from gui.json).
	RunWithoutAsking bool
	// ApproveAll is "Approve everything automatically" (AutoApproveAll reads
	// it from gui.json).
	ApproveAll bool
	// AutoDownload is "Download results automatically" (AutoDownloadResults
	// reads it from gui.json).
	AutoDownload bool
	// NotifyJobEnd and EmailOnJobEnd are "Notify me when a job ends" and
	// "Email me when a Slurm job ends" (M64.3; NotifyJobEnd, EmailOnJobEnd).
	NotifyJobEnd, EmailOnJobEnd bool
}

// View is every setting, for the page and /settings. Nothing in it is a
// secret.
type View struct {
	Site     SiteView     `json:"site"`
	Sites    []SiteChoice `json:"sites"`
	Clusters []Cluster    `json:"clusters"`
	// MoreClusters are clusters of the institution's profile that
	// clusters.yaml doesn't have (M57.2); the page offers to add them.
	MoreClusters []MoreCluster `json:"moreClusters"`
	ConfigError  string        `json:"configError"` // clusters.yaml couldn't be read; editing is off
	Hosts        []HostView    `json:"hosts"`
	SSHError     string        `json:"sshError"` // the SSH files couldn't be read
	App          AppView       `json:"app"`
	About        About         `json:"about"`
	// Restart: a change was saved this session and takes effect after
	// Labmill restarts.
	Restart bool `json:"restart"`
}

// MoreCluster is a cluster of the institution's profile that is not set up.
type MoreCluster struct {
	Name string `json:"name"`
	Type string `json:"type"` // "slurm"
}

// moreClusters lists the clusters of profile prof that clusters lacks
// (site.Site.MissingClusters: by name, or the same host under another name), in profile order. Never nil: an empty result is []. The generic
// profile has none.
func moreClusters(prof site.Site, clusters []config.Cluster, hostName func(alias string) string) []MoreCluster {
	out := []MoreCluster{}
	if prof.Domain == site.GenericDomain {
		return out
	}
	for _, t := range prof.MissingClusters(clusters, hostName) {
		out = append(out, MoreCluster{Name: t.Name, Type: string(t.Type)})
	}
	return out
}

// hostNameIn returns the HostName of an ssh alias in the user's SSH
// settings ("" when unknown).
func hostNameIn(p sshconfig.Paths) func(alias string) string {
	return func(alias string) string {
		if h, found, err := sshconfig.Lookup(p, alias); err == nil && found {
			return h.HostName
		}
		return ""
	}
}

// SiteView is the institution in use.
type SiteView struct {
	Domain  string `json:"domain"`
	Name    string `json:"name"`
	DocsURL string `json:"docsUrl"` // shown as text, never a link
}

// SiteChoice is one institution the user can switch to.
type SiteChoice struct {
	Domain string `json:"domain"`
	Name   string `json:"name"`
}

// Cluster is a clusters.yaml entry as the page edits it (S3 nil: no s3
// block). Credentials are not part of it: clusters.yaml never holds any.
type Cluster struct {
	Name      string `json:"name"`
	Type      string `json:"type"` // "slurm"
	SSHHost   string `json:"sshHost"`
	Transfer  string `json:"transfer"` // "", "sftp" or "s3"
	FilesHost string `json:"filesHost"`
	Timezone  string `json:"timezone"`
	// Site is the cluster's own institution domain ("" = the top-level
	// site); the page shows it and passes it back unchanged.
	Site string `json:"site"`
	S3   *S3    `json:"s3"`
}

// S3 is the s3 block (never keys: those come from the AWS profile).
type S3 struct {
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix"`
	Region   string `json:"region"`
	Profile  string `json:"profile"`
	Endpoint string `json:"endpoint"`
	SSE      string `json:"sse"`
	KMSKeyID string `json:"kmsKeyId"`
}

// HostView is one SSH connection.
type HostView struct {
	Alias     string `json:"alias"`
	HostName  string `json:"hostName"`
	User      string `json:"user"`
	Port      int    `json:"port"` // 0: the default (22)
	ProxyJump string `json:"proxyJump"`
	// InLabmill: defined in Labmill's own file and editable; false: the user's
	// own ~/.ssh/config defines it (read-only).
	InLabmill bool     `json:"inLabmill"`
	LoggedIn  bool     `json:"loggedIn"`
	UsedBy    []string `json:"usedBy"` // clusters (and hosts, as a jump host) that use it
}

// AppView is the desktop app's own settings.
type AppView struct {
	StartInTray  bool `json:"startInTray"`
	NoticesShown bool `json:"noticesShown"` // the one-time tray notice was shown
	// RunWithoutAsking: remote commands run without showing them first
	// (reads of sign-in secret files still ask).
	RunWithoutAsking bool `json:"runWithoutAsking"`
	// ApproveAll: every approval is given at once (M43.2); secret-file
	// reads, questions and logins still ask.
	ApproveAll bool `json:"approveAll"`
	// AutoDownload: results of a finished job come down by themselves
	// (M60.1); off by default.
	AutoDownload bool `json:"autoDownload"`
	// NotifyJobEnd: a desktop notification when a job ends (M64.3); on by
	// default. EmailOnJobEnd: Slurm scripts ask for mail at the end (off).
	NotifyJobEnd  bool `json:"notifyJobEnd"`
	EmailOnJobEnd bool `json:"emailOnJobEnd"`
}

// AutoApproveAll reads "Approve everything automatically" from the settings
// file at path (gui.json). A missing or unreadable file means off. The
// agent asks it at every approval, so a change applies at once.
func AutoApproveAll(path string) bool { return prefBool(path, "approveAll") }

// SetAutoApproveAll writes "Approve everything automatically" into the
// settings file at path, keeping every other key (the desktop app's other
// settings) as they are. The terminal's /autoapprove uses it; the file is
// written atomically with mode 0600.
func SetAutoApproveAll(path string, on bool) error { return setPrefBool(path, "approveAll", on) }

// AutoDownloadResults reads "Download results automatically" (M60.1) from
// the settings file at path (gui.json). A missing or unreadable file means
// off. The job monitor asks it whenever a job ends, so a change applies at
// once.
func AutoDownloadResults(path string) bool { return prefBool(path, "autoDownload") }

// SetAutoDownloadResults writes "Download results automatically" into the
// settings file at path like SetAutoApproveAll does. The terminal's
// /autodownload uses it.
func SetAutoDownloadResults(path string, on bool) error {
	return setPrefBool(path, "autoDownload", on)
}

// NotifyJobEnd reads "Notify me when a job ends" (M64.3) from the settings
// file at path (gui.json). Unlike the other switches it is ON when the key
// is missing (or the file can't be read).
func NotifyJobEnd(path string) bool {
	if b, err := os.ReadFile(path); err == nil {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			if v, ok := m["notifyJobEnd"].(bool); ok {
				return v
			}
		}
	}
	return true
}

// SetNotifyJobEnd writes "Notify me when a job ends" like SetAutoApproveAll.
func SetNotifyJobEnd(path string, on bool) error { return setPref(path, "notifyJobEnd", on) }

// EmailOnJobEnd reads "Email me when a Slurm job ends" (M64.3); off by
// default. It is read when a Slurm script is rendered.
func EmailOnJobEnd(path string) bool { return prefBool(path, "emailOnJobEnd") }

// SetEmailOnJobEnd writes "Email me when a Slurm job ends" like SetAutoApproveAll.
func SetEmailOnJobEnd(path string, on bool) error { return setPref(path, "emailOnJobEnd", on) }

// LastSeen reads the time Labmill was last running from its own small file
// at path (not gui.json, which the desktop app rewrites as a whole); the
// zero time when unknown. The job monitor keeps it up to date, and the
// "while you were away" notice only needs to know that a time exists.
func LastSeen(path string) time.Time {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	return t
}

// SetLastSeen saves the time Labmill was last running (temp file, then rename).
func SetLastSeen(path string, t time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lastseen-*")
	if err != nil {
		return err
	}
	_, werr := tmp.WriteString(t.UTC().Format(time.RFC3339) + "\n")
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		os.Remove(tmp.Name())
	}
	return werr
}

// setPrefBool writes one boolean key of gui.json, keeping every other key.
func setPrefBool(path, key string, on bool) error { return setPref(path, key, on) }

// setPref writes one key of gui.json, keeping every other key.
func setPref(path, key string, value any) error {
	if path == "" {
		return errors.New("Labmill can't find where to save its settings")
	}
	m := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	m[key] = value
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gui-*.json")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(b, '\n'))
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp.Name(), 0o600)
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return werr
	}
	return nil
}

// prefBool reads one boolean key of gui.json; anything unreadable is false.
func prefBool(path, key string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	v, _ := m[key].(bool)
	return v
}

// AutoRunRemote reads "Run commands on the cluster without asking" from
// the settings file at path (gui.json, Paths.Prefs). A missing or unreadable
// file means off: the safe default. Both the desktop app and the terminal
// ask it for every command, so a change applies at once.
func AutoRunRemote(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var p struct {
		RunWithoutAsking bool `json:"runWithoutAsking"`
	}
	if json.Unmarshal(b, &p) != nil {
		return false
	}
	return p.RunWithoutAsking
}

// About is the read-only diagnostics.
type About struct {
	Version   string      `json:"version"`
	Platform  string      `json:"platform"` // e.g. "linux/amd64"
	BrokerURL string      `json:"brokerUrl"`
	Files     []FileEntry `json:"files"`
	LogDir    string      `json:"logDir"`
}

// FileEntry is one of Labmill's files.
type FileEntry struct {
	Label string `json:"label"`
	Path  string `json:"path"`
}

// FieldError is a problem with one field of a form. Field is the
// clusters.yaml key ("name", "ssh_host", "s3.bucket", "site") or, for an SSH
// connection, "alias", "hostName", "user", "port" or "proxyJump".
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Result is the outcome of a save: OK, or the field errors to show next to
// the fields, or Error (a problem that isn't about one field).
type Result struct {
	OK      bool         `json:"ok"`
	Errors  []FieldError `json:"errors"`
	Error   string       `json:"error"`
	Message string       `json:"message"` // what happened, for a status line
	Restart bool         `json:"restart"` // the change takes effect after a restart
}

// RestartNote is the sentence a save that needs a restart ends with.
const RestartNote = "Restart Labmill to apply: quit it (tray menu > Quit Labmill) and open it again."

func (s *Service) goos() string {
	if s.GOOS != "" {
		return s.GOOS
	}
	return runtime.GOOS
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func failed(format string, args ...any) Result {
	return Result{Error: fmt.Sprintf(format, args...), Errors: []FieldError{}}
}

func fieldErrs(errs []FieldError) Result { return Result{Errors: errs} }

// readConfig reads clusters.yaml: its bytes (nil when missing) and parsed
// config (empty when missing).
func (s *Service) readConfig() ([]byte, *config.Config, error) {
	data, err := os.ReadFile(s.Paths.Config)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &config.Config{}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", s.Paths.Config, err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", s.Paths.Config, err)
	}
	return data, cfg, nil
}

// Get returns the current View.
func (s *Service) Get(info Info) View {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := View{Restart: s.changed, Clusters: []Cluster{}, Hosts: []HostView{}, Sites: []SiteChoice{}}
	_, cfg, err := s.readConfig()
	if err != nil {
		v.ConfigError = err.Error()
		cfg = &config.Config{}
	}
	st, serr := site.ForConfig(cfg)
	v.Site = SiteView{Domain: st.Domain, Name: st.Name, DocsURL: st.DocsURL}
	if serr != nil {
		dom := cfg.Site
		if dom == "" {
			dom = site.Default().Domain
		}
		v.Site = SiteView{Domain: dom, Name: dom + " (this Labmill has no profile for it; the generic one is used)"}
	}
	for _, c := range site.Choices() {
		v.Sites = append(v.Sites, SiteChoice{Domain: c.Domain, Name: c.Name})
	}
	v.MoreClusters = []MoreCluster{}
	if v.ConfigError == "" {
		prof := site.Site{}
		if serr == nil {
			prof = st
		}
		if s.Profile != nil {
			prof = s.Profile(cfg)
		}
		v.MoreClusters = moreClusters(prof, cfg.Clusters, hostNameIn(s.Paths.SSH))
	}
	for _, c := range cfg.Clusters {
		v.Clusters = append(v.Clusters, fromConfig(c))
	}
	entries, err := sshconfig.ListHosts(s.Paths.SSH)
	if err != nil {
		v.SSHError = err.Error()
	}
	for _, e := range entries {
		h := HostView{Alias: e.Alias, HostName: e.HostName, User: e.User, Port: e.Port, ProxyJump: e.ProxyJump,
			InLabmill: e.InLabmill, LoggedIn: slices.Contains(info.LoggedIn, e.Alias), UsedBy: usedBy(cfg, entries, e.Alias)}
		if h.Port == 22 {
			h.Port = 0
		}
		v.Hosts = append(v.Hosts, h)
	}
	v.App = AppView{StartInTray: info.StartInTray, NoticesShown: info.NoticesShown, RunWithoutAsking: info.RunWithoutAsking, ApproveAll: info.ApproveAll, AutoDownload: info.AutoDownload, NotifyJobEnd: info.NotifyJobEnd, EmailOnJobEnd: info.EmailOnJobEnd}
	v.About = About{
		Version:   info.Version,
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		BrokerURL: safeURL(info.BrokerURL),
		LogDir:    filepath.Dir(s.Paths.Log),
		Files: []FileEntry{
			{"Clusters (clusters.yaml)", s.Paths.Config},
			{"Labmill's SSH settings", s.Paths.SSH.LabmillConfig()},
			{"Your SSH settings (read-only here)", s.Paths.SSH.UserConfig()},
			{"Desktop app settings", s.Paths.Prefs},
			{"Job database", s.Paths.State},
			{"Log", s.Paths.Log},
		},
	}
	return v
}

// usedBy lists the clusters that use alias (ssh_host or files_host) and the
// hosts that jump through it.
func usedBy(cfg *config.Config, hosts []sshconfig.Entry, alias string) []string {
	out := []string{}
	for _, c := range cfg.Clusters {
		if c.SSHHost == alias || c.FilesHost == alias {
			out = append(out, c.Name)
		}
	}
	for _, h := range hosts {
		for _, hop := range strings.Split(h.ProxyJump, ",") {
			if jumpAlias(hop) == alias && h.Alias != alias {
				out = append(out, h.Alias+" (as its jump host)")
				break
			}
		}
	}
	return out
}

// jumpAlias is the host part of one ProxyJump hop ("user@host:port" ->
// "host").
func jumpAlias(hop string) string {
	hop = strings.TrimSpace(hop)
	if i := strings.LastIndex(hop, "@"); i >= 0 {
		hop = hop[i+1:]
	}
	if i := strings.LastIndex(hop, ":"); i >= 0 && !strings.Contains(hop, "]") {
		hop = hop[:i]
	}
	return hop
}

// safeURL drops any user info, query and fragment from a URL (none belongs
// in the broker address, and the page never shows one).
func safeURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(not a valid address)"
	}
	u.User, u.RawQuery, u.Fragment, u.ForceQuery = nil, "", "", false
	return u.String()
}

func fromConfig(c config.Cluster) Cluster {
	out := Cluster{Name: c.Name, Type: string(c.Type), SSHHost: c.SSHHost,
		Transfer: string(c.Transfer), FilesHost: c.FilesHost, Timezone: c.Timezone, Site: c.Site}
	if c.S3 != nil {
		out.S3 = &S3{Bucket: c.S3.Bucket, Prefix: c.S3.Prefix, Region: c.S3.Region, Profile: c.S3.Profile,
			Endpoint: c.S3.Endpoint, SSE: c.S3.SSE, KMSKeyID: c.S3.KMSKeyID}
	}
	return out
}

// toConfig trims every field; an s3 block whose fields are all empty is
// dropped.
func toConfig(c Cluster) config.Cluster {
	t := strings.TrimSpace
	out := config.Cluster{Name: t(c.Name), Type: config.ClusterType(t(c.Type)), SSHHost: t(c.SSHHost),
		Transfer: config.Transfer(t(c.Transfer)), FilesHost: t(c.FilesHost), Timezone: t(c.Timezone), Site: t(c.Site)}
	if c.S3 != nil {
		s3 := config.S3{Bucket: t(c.S3.Bucket), Prefix: t(c.S3.Prefix), Region: t(c.S3.Region), Profile: t(c.S3.Profile),
			Endpoint: t(c.S3.Endpoint), SSE: t(c.S3.SSE), KMSKeyID: t(c.S3.KMSKeyID)}
		if s3 != (config.S3{}) {
			out.S3 = &s3
		}
	}
	return out
}

// write backs clusters.yaml up (once per session) and writes data.
func (s *Service) write(data []byte) error {
	if s.backup == "" {
		b, err := config.Backup(s.Paths.Config, s.now())
		if err != nil {
			return err
		}
		s.backup = b
		if b == "" {
			s.backup = "-" // the file didn't exist: nothing to back up, ever
		}
	}
	if err := config.WriteFile(s.Paths.Config, data); err != nil {
		return err
	}
	s.changed = true
	return nil
}

// BackupPath returns the backup of clusters.yaml made this session ("" when
// none was needed yet).
func (s *Service) BackupPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backup == "-" {
		return ""
	}
	return s.backup
}

func saved(what string) Result {
	return Result{OK: true, Errors: []FieldError{}, Message: what + " " + RestartNote, Restart: true}
}

// SaveSite sets the institution (a domain from View.Sites). The clusters
// and SSH settings stay as they are.
func (s *Service) SaveSite(domain string) Result {
	domain = strings.TrimSpace(domain)
	if _, err := site.Get(domain); err != nil {
		return fieldErrs([]FieldError{{Field: "site", Message: "Choose your institution from the list."}})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, cfg, err := s.readConfig()
	if err != nil {
		return failed("%v", err)
	}
	if cfg.Site == domain { // an empty site is not a home institution (M55): record the choice
		return Result{OK: true, Errors: []FieldError{}, Message: "Nothing changed."}
	}
	out, err := config.SetSiteYAML(data, domain)
	if err != nil {
		return failed("couldn't update clusters.yaml: %v", err)
	}
	if err := s.write(out); err != nil {
		return failed("%v", err)
	}
	return saved("Saved. Your clusters and connections stay as they are; the institution's help, skills and laptop policy change.")
}

// SaveCluster saves c in place of the cluster named oldName ("" adds c as a
// new cluster). The whole configuration with the change is checked with
// config.FieldErrors; the problems of this cluster come back per field. The
// type-specific fields of the other type are dropped, and an s3 block with
// no values is removed.
func (s *Service) SaveCluster(oldName string, c Cluster) Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, cfg, err := s.readConfig()
	if err != nil {
		return failed("%v", err)
	}
	nc := toConfig(c)
	next := &config.Config{Site: cfg.Site, Clusters: append([]config.Cluster(nil), cfg.Clusters...)}
	idx := -1
	if oldName != "" {
		for i, x := range next.Clusters {
			if x.Name == oldName {
				idx = i
			}
		}
		if idx < 0 {
			return failed("cluster %s is no longer configured (reopen Settings)", oldName)
		}
		// The page doesn't edit the cluster's institution: keep it.
		if nc.Site == "" {
			nc.Site = next.Clusters[idx].Site
		}
		next.Clusters[idx] = nc
	} else {
		next.Clusters = append(next.Clusters, nc)
		idx = len(next.Clusters) - 1
	}
	var errs []FieldError
	// A name another cluster has: Validate reports the duplicate on the
	// later entry, which may be the other one; it belongs on this form.
	for j, x := range next.Clusters {
		if j != idx && nc.Name != "" && x.Name == nc.Name {
			errs = append(errs, FieldError{Field: "name", Message: "Another cluster is already named " + nc.Name + "; choose another name."})
			break
		}
	}
	for _, fe := range next.FieldErrors() {
		if fe.Field == "name" && fe.Message == "duplicate name" {
			continue // reported above
		}
		if fe.Cluster == idx {
			errs = append(errs, FieldError{Field: fe.Field, Message: fe.Message})
		}
	}
	if len(errs) > 0 {
		return fieldErrs(errs)
	}
	out, err := config.PutClusterYAML(data, oldName, nc)
	if err != nil {
		return failed("couldn't update clusters.yaml: %v", err)
	}
	if err := s.write(out); err != nil {
		return failed("%v", err)
	}
	msg := "Saved cluster " + nc.Name + "."
	if oldName != "" && oldName != nc.Name {
		msg += " Jobs started before keep the old name " + oldName + "."
	}
	return saved(msg)
}

// RemoveCluster removes the cluster named name from clusters.yaml.
func (s *Service) RemoveCluster(name string) Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, cfg, err := s.readConfig()
	if err != nil {
		return failed("%v", err)
	}
	if _, ok := cfg.Cluster(name); !ok {
		return failed("cluster %s is no longer configured (reopen Settings)", name)
	}
	out, err := config.RemoveClusterYAML(data, name)
	if err != nil {
		return failed("couldn't update clusters.yaml: %v", err)
	}
	if err := s.write(out); err != nil {
		return failed("%v", err)
	}
	return saved("Removed cluster " + name + ".")
}

// hostFieldErrors checks h field by field with the rules of
// sshconfig.Host.Validate (plus: a host name is required).
func hostFieldErrors(h sshconfig.Host) []FieldError {
	var errs []FieldError
	if err := sshconfig.ValidAlias(h.Alias); err != nil {
		errs = append(errs, FieldError{"alias", "Use a short name without spaces or any of * ? ! , # \" ' = (" + err.Error() + ")."})
	}
	if h.HostName == "" {
		errs = append(errs, FieldError{"hostName", "Enter the host name, for example login.example.edu."})
	}
	for _, f := range []struct {
		field string
		h     sshconfig.Host
	}{
		{"hostName", sshconfig.Host{Alias: "x", HostName: h.HostName}},
		{"user", sshconfig.Host{Alias: "x", User: h.User}},
		{"proxyJump", sshconfig.Host{Alias: "x", ProxyJump: h.ProxyJump}},
	} {
		if err := f.h.Validate(); err != nil {
			msg := err.Error()
			if i := strings.Index(msg, ": "); i >= 0 {
				msg = msg[i+2:]
			}
			errs = append(errs, FieldError{f.field, msg})
		}
	}
	if h.Port < 0 || h.Port > 65535 {
		errs = append(errs, FieldError{"port", "The port is a number from 1 to 65535 (empty means 22)."})
	}
	return errs
}

// SaveHost saves an SSH connection in Labmill's own file: oldAlias "" adds a
// new one (its alias must not be defined in either file: an entry in
// Labmill's file would override the user's own), otherwise it edits the
// Labmill entry oldAlias (the alias itself can't change, since clusters refer
// to it). The user's own entries can't be saved here.
func (s *Service) SaveHost(oldAlias string, h HostView) Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := strings.TrimSpace
	host := sshconfig.Host{Alias: t(h.Alias), HostName: t(h.HostName), User: t(h.User), Port: h.Port, ProxyJump: t(h.ProxyJump)}
	if host.Port == 22 {
		host.Port = 0
	}
	entries, err := sshconfig.ListHosts(s.Paths.SSH)
	if err != nil {
		return failed("couldn't read the SSH settings: %v", err)
	}
	var cur *sshconfig.Entry
	for i := range entries {
		if entries[i].Alias == host.Alias || (oldAlias != "" && entries[i].Alias == oldAlias) {
			if cur == nil || entries[i].Alias == oldAlias {
				cur = &entries[i]
			}
		}
	}
	// A new alias must not equal any Host pattern of either file, compared
	// as OpenSSH does (case-insensitively, multi-pattern blocks included):
	// Labmill's file is read first and would override the user's own entry.
	taken := false
	if oldAlias == "" && host.Alias != "" && sshconfig.ValidAlias(host.Alias) == nil {
		free, err := sshconfig.UniqueAlias(s.Paths.SSH, host.Alias)
		if err != nil {
			return failed("couldn't read the SSH settings: %v", err)
		}
		taken = free != host.Alias
	}
	inLabmill := false
	for _, e := range entries {
		if e.InLabmill && strings.EqualFold(e.Alias, host.Alias) {
			inLabmill = true
		}
	}
	var errs []FieldError
	switch {
	case oldAlias == "" && taken && inLabmill:
		errs = append(errs, FieldError{"alias", "Labmill already has a connection named " + host.Alias + "; edit that one or choose another name."})
	case oldAlias == "" && taken:
		errs = append(errs, FieldError{"alias", "Your SSH settings already use the name " + host.Alias + " (names count as the same regardless of upper or lower case); choose another name (Labmill never overrides your own entries)."})
	case oldAlias != "" && (cur == nil || cur.Alias != oldAlias):
		return failed("%s is no longer in Labmill's SSH settings (reopen Settings)", oldAlias)
	case oldAlias != "" && !cur.InLabmill:
		return failed("%s is defined in your own SSH settings, which Labmill only reads; edit it in %s", oldAlias, s.Paths.SSH.UserConfig())
	case oldAlias != "" && host.Alias != oldAlias:
		errs = append(errs, FieldError{"alias", "The name of a connection can't change (clusters refer to it); add a new connection instead."})
	}
	errs = append(errs, hostFieldErrors(host)...)
	if len(errs) > 0 {
		return fieldErrs(errs)
	}
	if cur != nil && cur.InLabmill {
		host.IdentityFiles = cur.IdentityFiles // kept as they are (PutLabmillHost doesn't touch those lines)
	}
	created, err := sshconfig.PutLabmillHost(s.Paths.SSH, host, s.goos())
	if err != nil {
		return failed("couldn't update Labmill's SSH settings: %v", err)
	}
	if created {
		if _, err := sshconfig.EnsureInclude(s.Paths.SSH, s.now()); err != nil {
			return failed("saved the connection, but couldn't add the Include line to %s: %v", s.Paths.SSH.UserConfig(), err)
		}
	}
	s.changed = true
	return saved("Saved connection " + host.Alias + ". It is used for the next login.")
}

// RemoveHost removes an SSH connection from Labmill's own file. A connection
// a cluster uses (or another connection jumps through) is kept.
func (s *Service) RemoveHost(alias string) Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := sshconfig.ListHosts(s.Paths.SSH)
	if err != nil {
		return failed("couldn't read the SSH settings: %v", err)
	}
	var cur *sshconfig.Entry
	for i := range entries {
		if entries[i].Alias == alias {
			cur = &entries[i]
		}
	}
	if cur == nil || !cur.InLabmill {
		return failed("%s isn't in Labmill's SSH settings, so there is nothing to remove there", alias)
	}
	_, cfg, err := s.readConfig()
	if err != nil {
		return failed("%v", err)
	}
	if users := usedBy(cfg, entries, alias); len(users) > 0 {
		sort.Strings(users)
		return failed("%s is still used by %s; change those first", alias, strings.Join(users, ", "))
	}
	if err := sshconfig.RemoveLabmillHost(s.Paths.SSH, alias); err != nil {
		return failed("couldn't update Labmill's SSH settings: %v", err)
	}
	s.changed = true
	return saved("Removed connection " + alias + ".")
}
