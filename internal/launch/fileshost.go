package launch

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"labmill-client/internal/agent"
	"labmill-client/internal/config"
	"labmill-client/internal/site"
	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx"
)

// filesHostOffer is a transfer host the site profile defines for a Slurm
// cluster whose clusters.yaml (written before files_host existed) has none.
type filesHostOffer struct {
	Cluster  string         // the cluster's name in clusters.yaml
	Template sshconfig.Host // the site profile's transfer host (Alias is the template alias)
}

// filesHostAskedPath is the file that remembers which clusters were already
// asked about a transfer host, one name per line.
func filesHostAskedPath(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), "files-host-asked")
}

// readFilesHostAsked returns the cluster names already asked about.
func readFilesHostAsked(cfgPath string) map[string]bool {
	asked := map[string]bool{}
	data, err := os.ReadFile(filesHostAskedPath(cfgPath))
	if err != nil {
		return asked
	}
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			asked[l] = true
		}
	}
	return asked
}

// markFilesHostAsked remembers that cluster was asked about.
func markFilesHostAsked(cfgPath, cluster string) error {
	path := filesHostAskedPath(cfgPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintln(f, cluster)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// pendingFilesHosts lists the Slurm clusters of cfg that have no files_host
// while s defines a transfer host (RoleTransfer, reached directly: no
// ProxyJump) for the cluster of the same name, and that are not in asked.
func pendingFilesHosts(s site.Site, cfg *config.Config, asked map[string]bool) []filesHostOffer {
	var out []filesHostOffer
	for _, c := range cfg.Clusters {
		if c.Type != config.TypeSlurm || c.FilesHost != "" || asked[c.Name] {
			continue
		}
		for _, tc := range s.Clusters {
			if tc.Name != c.Name || tc.FilesHost == "" {
				continue
			}
			for _, h := range s.Hosts {
				if h.Alias == tc.FilesHost && h.HostName != "" && h.ProxyJump == "" && s.Roles[h.Alias] == site.RoleTransfer {
					out = append(out, filesHostOffer{Cluster: c.Name, Template: h})
				}
			}
			break
		}
	}
	return out
}

// backupConfig copies cfgPath to "<cfgPath>.labmill-backup-<now>", adding a
// numeric suffix when that name exists, so backups never overwrite each other.
// A missing cfgPath needs no backup.
func backupConfig(cfgPath string, now time.Time) error {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil
	}
	base := cfgPath + ".labmill-backup-" + now.Format("20060102-150405")
	for i := 0; i < 1000; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("back up %s: %w", cfgPath, err)
		}
		_, werr := f.Write(data)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return fmt.Errorf("back up %s: %w", cfgPath, werr)
		}
		return nil
	}
	return fmt.Errorf("back up %s: too many backups with the same name", cfgPath)
}

// setFilesHostYAML inserts (or replaces) files_host: alias in the mapping of
// the cluster named cluster, leaving everything else in data (comments, key
// order) as it is. The result is validated as a config.
func setFilesHostYAML(data []byte, cluster, alias string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse clusters.yaml: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("clusters.yaml has an unexpected layout")
	}
	root := doc.Content[0]
	var target *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "clusters" || root.Content[i+1].Kind != yaml.SequenceNode {
			continue
		}
		for _, c := range root.Content[i+1].Content {
			if c.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j+1 < len(c.Content); j += 2 {
				if c.Content[j].Value == "name" && c.Content[j+1].Value == cluster {
					target = c
				}
			}
		}
	}
	if target == nil {
		return nil, fmt.Errorf("cluster %s is no longer configured", cluster)
	}
	val := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: alias}
	replaced := false
	for j := 0; j+1 < len(target.Content); j += 2 {
		if target.Content[j].Value == "files_host" {
			target.Content[j+1] = val
			replaced = true
		}
	}
	if !replaced {
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "files_host"}
		pos := len(target.Content)
		for j := 0; j+1 < len(target.Content); j += 2 {
			if target.Content[j].Value == "ssh_host" {
				pos = j + 2
			}
		}
		target.Content = append(target.Content[:pos], append([]*yaml.Node{key, val}, target.Content[pos:]...)...)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("encode clusters.yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode clusters.yaml: %w", err)
	}
	var check config.Config
	if err := yaml.Unmarshal(buf.Bytes(), &check); err != nil {
		return nil, fmt.Errorf("re-read clusters.yaml: %w", err)
	}
	if err := check.Validate(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// applyFilesHost sets files_host on the cluster o.Cluster in the file cfgPath.
// The transfer host's SSH alias is reused when the OpenSSH config already has
// one for its HostName; otherwise a new entry (with the login host's user) is
// appended to Labmill's ssh file. Only the file changes, never cfg, which other
// goroutines read: the new setting takes effect the next time Labmill starts.
// Only that one key is inserted, so comments and key order in clusters.yaml
// survive. The caller backs the file up first (backupConfig).
func applyFilesHost(p sshconfig.Paths, cfgPath string, cfg *config.Config, o filesHostOffer, goos string, now time.Time) error {
	idx := -1
	for i, c := range cfg.Clusters {
		if c.Name == o.Cluster {
			idx = i
		}
	}
	if idx < 0 {
		return fmt.Errorf("cluster %s is no longer configured", o.Cluster)
	}
	alias, _, found, err := sshconfig.AliasSource(p, o.Template.HostName)
	if err != nil {
		return fmt.Errorf("read ssh settings: %w", err)
	}
	if !found {
		if alias, err = sshconfig.UniqueAlias(p, o.Template.Alias); err != nil {
			return fmt.Errorf("choose a name for %s: %w", o.Template.HostName, err)
		}
		h := o.Template
		h.Alias = alias
		if login := cfg.Clusters[idx].SSHHost; login != "" {
			if lh, _, lerr := sshconfig.Lookup(p, login); lerr == nil {
				h.User = lh.User
			}
		}
		if err := h.Validate(); err != nil {
			return err
		}
		if err := appendHost(p, h, goos); err != nil {
			return err
		}
		if _, err := sshconfig.EnsureInclude(p, now); err != nil {
			return fmt.Errorf("update ssh settings: %w", err)
		}
	}
	path := cfgPath
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if t, err := filepath.EvalSymlinks(path); err == nil {
			path = t
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", cfgPath, err)
	}
	out, err := setFilesHostYAML(data, o.Cluster, alias)
	if err != nil {
		return err
	}
	return sshconfig.AtomicWrite(path, out)
}

// appendHost adds one host block to Labmill's ssh file, keeping its other
// entries (sshconfig.WriteHosts would replace the whole file).
func appendHost(p sshconfig.Paths, h sshconfig.Host, goos string) error {
	path := p.LabmillConfig()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	block := sshconfig.Render([]sshconfig.Host{h}, goos)
	if len(old) == 0 {
		old = nil
	} else {
		// Drop Render's header comment: the file already has one.
		if i := bytes.IndexByte(block, '\n'); i >= 0 {
			block = block[i+1:]
		}
		if !bytes.HasSuffix(old, []byte("\n")) {
			old = append(old, '\n')
		}
	}
	if err := sshconfig.AtomicWrite(path, append(old, block...)); err != nil {
		return err
	}
	if goos != "windows" {
		_ = os.MkdirAll(filepath.Join(p.SSHDir, "controlmasters"), 0o700)
	}
	return nil
}

// offerFilesHosts asks, once per cluster, whether to use the site's transfer
// host (see pendingFilesHosts). Yes updates clusters.yaml (applyFilesHost);
// yes and no are remembered; a dismissed question is asked again next
// start. A cluster is remembered only after a successful apply (or a No), so
// a failed Yes is offered again. clusters.yaml is backed up at most once per
// run, and cfg is not modified. It returns lines to log (failures), and text to tell the user.
func offerFilesHosts(ctx context.Context, ch agent.Chooser, p sshconfig.Paths, cfgPath string, s site.Site, cfg *config.Config, goos string) (notices []string) {
	backedUp := false
	for _, o := range pendingFilesHosts(s, cfg, readFilesHostAsked(cfgPath)) {
		q := fmt.Sprintf("Transfers to %s will use %s, the site's dedicated transfer host (better for large data), if you add files_host to clusters.yaml. Until then they go through the login host. Add it? (clusters.yaml is backed up first. You'll approve one more %s when the transfer host is first used.)",
			o.Cluster, o.Template.HostName, sshx.SignInRequest(s.TwoFactorName))
		a, err := ch.Choose(ctx, agent.Choice{Question: q, Options: []agent.ChoiceOption{
			{Label: "Yes, use " + o.Template.HostName, Description: "Updates clusters.yaml; big transfers get faster."},
			{Label: "No, keep using the login host"},
		}})
		if err != nil || a.Skipped || a.Other != "" {
			continue
		}
		if a.Index == 0 {
			if !backedUp {
				if err := backupConfig(cfgPath, time.Now()); err != nil {
					notices = append(notices, fmt.Sprintf("couldn't add the transfer host for %s: %v", o.Cluster, err))
					continue
				}
				backedUp = true
			}
			if err := applyFilesHost(p, cfgPath, cfg, o, goos, time.Now()); err != nil {
				notices = append(notices, fmt.Sprintf("couldn't add the transfer host for %s: %v", o.Cluster, err))
				continue
			}
			notices = append(notices, fmt.Sprintf("Transfers to %s will use %s the next time Labmill starts.", o.Cluster, o.Template.HostName))
		}
		if err := markFilesHostAsked(cfgPath, o.Cluster); err != nil {
			notices = append(notices, fmt.Sprintf("couldn't remember the answer for %s: %v", o.Cluster, err))
		}
	}
	return notices
}
