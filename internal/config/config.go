// Package config loads, validates and saves Labmill's cluster configuration,
// ~/.config/labmill/clusters.yaml.
//
// SSH connection details (host name, user, port, jump host) are deliberately
// not stored here: Slurm clusters refer to an alias in the OpenSSH-format file
// managed by package sshconfig, so the user's terminal ssh and Labmill share one
// source of truth.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // zone database for Windows, which has none

	"go.yaml.in/yaml/v3"

	"labmill-client/internal/sshconfig"
)

// ClusterType selects the backend for a cluster.
type ClusterType string

// TypeSlurm is the only backend.
const TypeSlurm ClusterType = "slurm"

// Transfer selects how local data reaches the cluster.
type Transfer string

const (
	// TransferSFTP uploads over SFTP on the cluster's existing SSH connection
	// (the bastion/login host). It is the default: no extra login, no setup.
	TransferSFTP Transfer = "sftp"
	// TransferS3 stages data in an S3 bucket. Faster, but needs configuration.
	TransferS3 Transfer = "s3"
)

// Cluster is one entry in clusters.yaml.
type Cluster struct {
	// Name is the user-facing, unique cluster name.
	Name string `yaml:"name"`
	// Type is the backend: TypeSlurm.
	Type ClusterType `yaml:"type"`
	// SSHHost is an alias in ~/.ssh/config.d/labmill (or the user's own
	// ~/.ssh/config). Required.
	SSHHost string `yaml:"ssh_host,omitempty"`
	// Transfer is how data is uploaded. Empty means TransferSFTP.
	Transfer Transfer `yaml:"transfer,omitempty"`
	// FilesHost optionally names an SSH alias for a dedicated transfer host
	// (e.g. files.example.edu), used for all file transfers when set (for login hosts without sftp).
	FilesHost string `yaml:"files_host,omitempty"`
	// Timezone is an IANA time zone (e.g. "America/Los_Angeles") used to read
	// the zone-less timestamps sacct prints. Labmill runs sacct with TZ=UTC, so
	// the times already are UTC and this is rarely needed: set it only if the
	// cluster's Slurm ignores TZ. Optional, Slurm only; empty means UTC.
	Timezone string `yaml:"timezone,omitempty"`
	// S3 configures the S3 staging path. Required when Transfer is TransferS3;
	// with any other transfer it is allowed (so users can keep it ready) and
	// ignored, but still validated.
	S3 *S3 `yaml:"s3,omitempty"`
	// Site is the domain of the institution this cluster belongs to (e.g.
	// "example.edu"); empty means the top-level Config.Site. A cluster of
	// another institution is added on the user's explicit request; its
	// hosts, 2FA text and site skill come from that institution's profile.
	Site string `yaml:"site,omitempty"`
}

// S3 configures the S3 staging path (Transfer == TransferS3). Credentials
// never go in clusters.yaml: Labmill uses the AWS SDK's default chain for the
// named profile (~/.aws/config and credentials, SSO, env).
type S3 struct {
	Bucket   string `yaml:"bucket"`             // required; S3 bucket naming rules
	Prefix   string `yaml:"prefix,omitempty"`   // key prefix, no leading '/', no ".." segments
	Region   string `yaml:"region,omitempty"`   // e.g. us-west-2; empty = from the profile
	Profile  string `yaml:"profile,omitempty"`  // AWS profile name; empty = default chain
	Endpoint string `yaml:"endpoint,omitempty"` // optional https URL for S3-compatible storage
	// SSE is the server-side encryption to request: "" (bucket default),
	// "AES256" (SSE-S3) or "aws:kms" (SSE-KMS, for buckets that require it).
	SSE string `yaml:"sse,omitempty"`
	// KMSKeyID is the KMS key ID, ARN or alias to use; only with sse aws:kms
	// (empty = the bucket's default KMS key).
	KMSKeyID string `yaml:"kms_key_id,omitempty"`
}

// String returns "s3://bucket/prefix" (or "s3://bucket" without a prefix).
func (s S3) String() string {
	if s.Prefix == "" {
		return "s3://" + s.Bucket
	}
	return "s3://" + s.Bucket + "/" + strings.TrimRight(s.Prefix, "/")
}

var (
	bucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	ipRe     = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)
	regionRe = regexp.MustCompile(`^[a-z0-9-]+$`)
	// kmsKeyRe accepts a key ID (UUID), a key or alias ARN and "alias/name".
	kmsKeyRe = regexp.MustCompile(`^[A-Za-z0-9:/_+=.@-]{1,1000}$`)
)

var s3FieldRe = regexp.MustCompile(`field (\S+) not found in type config\.S3`)

// s3FieldError explains an unknown key in the s3 block without Go type names.
func s3FieldError(field string) error {
	low := strings.ToLower(field)
	for _, w := range []string{"access_key", "secret", "session_token", "token", "password", "key_id", "credential"} {
		if strings.Contains(low, w) {
			return fmt.Errorf("parse clusters.yaml: `%s` in the s3 block: credentials don't belong in clusters.yaml: Labmill reads them from your AWS profile; set s3.profile", field)
		}
	}
	return fmt.Errorf("parse clusters.yaml: unknown setting `%s` in the s3 block of a cluster (allowed: bucket, prefix, region, profile, endpoint, sse, kms_key_id)", field)
}

// s3Problem is one problem with the s3 block: its field ("s3.bucket") and
// the plain message.
type s3Problem struct{ field, msg string }

// problems reports every problem with the s3 block.
func (s S3) problems() []s3Problem {
	var out []s3Problem
	switch {
	case s.Bucket == "":
		out = append(out, s3Problem{"s3.bucket", "s3.bucket is required"})
	case !bucketRe.MatchString(s.Bucket) || strings.Contains(s.Bucket, "..") || ipRe.MatchString(s.Bucket):
		out = append(out, s3Problem{"s3.bucket", fmt.Sprintf("s3.bucket %q is not a valid bucket name (3-63 lowercase letters, digits, dots or dashes, starting and ending with a letter or digit)", s.Bucket)})
	}
	if s.Prefix != "" {
		bad := strings.HasPrefix(s.Prefix, "/") || strings.ContainsAny(s.Prefix, "\\\x00")
		for _, seg := range strings.Split(s.Prefix, "/") {
			if seg == ".." {
				bad = true
			}
		}
		for _, r := range s.Prefix {
			if r < ' ' || r == 0x7f {
				bad = true
			}
		}
		if bad {
			out = append(out, s3Problem{"s3.prefix", fmt.Sprintf("s3.prefix %q must not start with '/' or contain '..' or control characters", s.Prefix)})
		}
	}
	if s.Region != "" && !regionRe.MatchString(s.Region) {
		out = append(out, s3Problem{"s3.region", fmt.Sprintf("s3.region %q may only contain lowercase letters, digits and dashes", s.Region)})
	}
	if strings.ContainsAny(s.Profile, "\n\r\x00") {
		out = append(out, s3Problem{"s3.profile", "s3.profile contains an unusual character"})
	}
	switch s.SSE {
	case "", "AES256", "aws:kms":
	default:
		out = append(out, s3Problem{"s3.sse", fmt.Sprintf("s3.sse %q must be AES256 or aws:kms", s.SSE)})
	}
	if s.KMSKeyID != "" {
		switch {
		case s.SSE != "aws:kms":
			out = append(out, s3Problem{"s3.kms_key_id", "s3.kms_key_id needs s3.sse: aws:kms"})
		case !kmsKeyRe.MatchString(s.KMSKeyID):
			out = append(out, s3Problem{"s3.kms_key_id", fmt.Sprintf("s3.kms_key_id %q must be a KMS key ID, key ARN or alias (no spaces)", s.KMSKeyID)})
		}
	}
	if s.Endpoint != "" {
		u, err := url.Parse(s.Endpoint)
		local := err == nil && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")
		switch {
		case err != nil || u.Hostname() == "" || (u.Scheme != "https" && !(u.Scheme == "http" && local)):
			out = append(out, s3Problem{"s3.endpoint", fmt.Sprintf("s3.endpoint %q must be an https URL", s.Endpoint)})
		case u.User != nil:
			out = append(out, s3Problem{"s3.endpoint", "s3.endpoint must not contain a user name or password"})
		case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
			out = append(out, s3Problem{"s3.endpoint", fmt.Sprintf("s3.endpoint %q must not contain a query string or fragment", s.Endpoint)})
		}
	}
	return out
}

// Location returns the cluster's time zone: UTC when Timezone is empty,
// otherwise time.LoadLocation(Timezone). "Local" is rejected because it
// would depend on the machine Labmill runs on.
func (c Cluster) Location() (*time.Location, error) {
	if c.Timezone == "" {
		return time.UTC, nil
	}
	if strings.EqualFold(c.Timezone, "local") {
		return nil, errors.New(`time zone "Local" is not allowed; use an IANA name such as "America/Los_Angeles"`)
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return nil, fmt.Errorf("time zone %q: %w", c.Timezone, err)
	}
	return loc, nil
}

// EffectiveTransfer returns c.Transfer, or TransferSFTP when it is empty.
func (c Cluster) EffectiveTransfer() Transfer {
	if c.Transfer == "" {
		return TransferSFTP
	}
	return c.Transfer
}

// Config is the whole clusters.yaml document.
type Config struct {
	// Site is the institution's domain chosen at first run (a sites/
	// directory name, e.g. "example.edu", or "generic"): the home
	// institution, required (M55). Empty or unknown sends Labmill back to the
	// institution form. It selects the site profile (hosts, policy, broker)
	// and the site skill.
	Site     string    `yaml:"site,omitempty"`
	Clusters []Cluster `yaml:"clusters"`

	// mu guards Clusters once a session shares the Config: add_cluster
	// (AddCluster) appends while the job monitor and the front ends read
	// (Cluster, List). Code that fills a Config before sharing it, and
	// Parse/Save, use Clusters directly.
	mu sync.RWMutex
}

// List returns a copy of the clusters, safe to call while AddCluster runs.
func (c *Config) List() []Cluster {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]Cluster(nil), c.Clusters...)
}

// AddCluster appends cl unless a cluster of that name exists; it reports
// whether it was added.
func (c *Config) AddCluster(cl Cluster) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, x := range c.Clusters {
		if x.Name == cl.Name {
			return false
		}
	}
	// A new slice, so a List caller's copy and any older header stay valid.
	c.Clusters = append(append([]Cluster(nil), c.Clusters...), cl)
	return true
}

// siteRE matches a lowercase domain name or "generic" (the site package checks
// that the site is known).
var siteRE = regexp.MustCompile(`^(generic|[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+)$`)

// ErrNoConfig is returned (wrapped) by Load when the file does not exist, so
// callers can start the first-run setup instead of failing.
var ErrNoConfig = errors.New("no Labmill configuration yet")

// DefaultPath returns the path of clusters.yaml: $XDG_CONFIG_HOME/labmill/
// clusters.yaml if XDG_CONFIG_HOME is set, otherwise
// <home>/.config/labmill/clusters.yaml on every OS, including macOS and
// Windows, so the README's documented location holds everywhere.
func DefaultPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find home directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "labmill", "clusters.yaml"), nil
}

// Parse decodes and validates a clusters.yaml document. Unknown fields are an
// error (so typos surface), and so is any problem Validate reports.
func Parse(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) { // io.EOF: empty document
		if m := s3FieldRe.FindStringSubmatch(err.Error()); m != nil {
			return nil, s3FieldError(m[1])
		}
		return nil, fmt.Errorf("parse clusters.yaml: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Load reads and parses the file at path. A missing file yields an error that
// wraps both ErrNoConfig and fs.ErrNotExist.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %w", ErrNoConfig, err)
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Save validates c and writes it to path atomically (temp file in the same
// directory, then rename), creating parent directories with mode 0700 and
// the file with mode 0600.
func Save(path string, c *Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return fmt.Errorf("encode clusters.yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("encode clusters.yaml: %w", err)
	}
	// Write through a symlinked file instead of replacing the link.
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("resolve symlink %s: %w", path, err)
		}
		path = target
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".clusters-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmpName) // our own temp file
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fail(err)
	}
	return nil
}

// FieldError is one problem Validate found, tied to the setting it is about,
// so a settings form can show it next to the field.
type FieldError struct {
	// Cluster is the 0-based index of the cluster in Config.Clusters, or -1
	// for a top-level setting (site).
	Cluster int
	// Who names the cluster as in the message: `cluster "x"`, or `cluster 3`
	// when it has no name; "" for a top-level setting.
	Who string
	// Field is the clusters.yaml key: "site", "name", "type", "ssh_host",
	// "transfer", "files_host", "timezone", "site", "s3"
	// (the block as a whole) or "s3.<key>" (e.g. "s3.bucket").
	Field string
	// Message is the problem without the cluster prefix, e.g. "slurm cluster
	// needs ssh_host".
	Message string
}

// Error is the message as Validate reports it: "<who>: <message>", or the
// message alone for a top-level setting.
func (e FieldError) Error() string {
	if e.Who == "" {
		return e.Message
	}
	return e.Who + ": " + e.Message
}

// Validate reports every problem at once (joined with errors.Join):
//   - a cluster without a name, or a duplicate name
//   - an unknown type
//   - a Slurm cluster without ssh_host
//   - an unknown transfer value, a missing s3 block for transfer s3, or an invalid
//     s3 block (an s3 block next to another transfer is allowed and ignored)
//   - a timezone that is not a loadable IANA name
//   - an ssh_host or files_host that is not a valid single SSH alias
//   - a site (top-level or of a cluster) that is not a lowercase domain name (or "generic")
//
// Each message names the offending cluster (or its index if it has no name).
// The joined errors are FieldError values (see FieldErrors).
func (c *Config) Validate() error {
	var errs []error
	for _, fe := range c.FieldErrors() {
		errs = append(errs, fe)
	}
	return errors.Join(errs...)
}

// FieldErrors returns the problems Validate reports, in the same order, each
// tied to its cluster and field.
func (c *Config) FieldErrors() []FieldError {
	var errs []FieldError
	if c.Site != "" && !siteRE.MatchString(c.Site) {
		errs = append(errs, FieldError{Cluster: -1, Field: "site", Message: fmt.Sprintf("site %q must be an institution domain like example.edu (or generic)", c.Site)})
	}
	seen := map[string]bool{}
	for i, cl := range c.Clusters {
		who := fmt.Sprintf("cluster %d", i+1)
		if cl.Name != "" {
			who = fmt.Sprintf("cluster %q", cl.Name)
		}
		bad := func(field, format string, args ...any) {
			errs = append(errs, FieldError{Cluster: i, Who: who, Field: field, Message: fmt.Sprintf(format, args...)})
		}
		switch {
		case cl.Name == "":
			bad("name", "missing name")
		case seen[cl.Name]:
			bad("name", "duplicate name")
		}
		if cl.Name != "" {
			seen[cl.Name] = true
		}
		switch cl.Type {
		case TypeSlurm:
			if cl.SSHHost == "" {
				bad("ssh_host", "slurm cluster needs ssh_host")
			}
		default:
			bad("type", "unknown type %q (want %q)", string(cl.Type), TypeSlurm)
		}
		if cl.SSHHost != "" {
			if err := sshconfig.ValidAlias(cl.SSHHost); err != nil {
				bad("ssh_host", "ssh_host: %v", err)
			}
		}
		if cl.Timezone != "" {
			if _, err := cl.Location(); err != nil {
				bad("timezone", "timezone: %v", err)
			}
		}
		if cl.FilesHost != "" {
			if err := sshconfig.ValidAlias(cl.FilesHost); err != nil {
				bad("files_host", "files_host: %v", err)
			}
		}
		if cl.Site != "" && !siteRE.MatchString(cl.Site) {
			bad("site", "site %q must be an institution domain like example.edu (or generic)", cl.Site)
		}
		switch cl.Transfer {
		case "", TransferSFTP, TransferS3:
		default:
			bad("transfer", "unknown transfer %q (want %q or %q)", string(cl.Transfer), TransferSFTP, TransferS3)
		}
		switch {
		case cl.Transfer == TransferS3 && cl.S3 == nil:
			bad("s3", "transfer is s3 but there is no s3 block (needs at least bucket)")
		case cl.S3 != nil:
			for _, p := range cl.S3.problems() {
				bad(p.field, "%s", p.msg)
			}
		}
	}
	return errs
}

// Cluster returns the cluster with the given name.
func (c *Config) Cluster(name string) (Cluster, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, cl := range c.Clusters {
		if cl.Name == name {
			return cl, true
		}
	}
	return Cluster{}, false
}
