package settings

import (
	"fmt"
	"strings"
)

// Text renders v for the terminal's /settings command: every value (no
// secrets) and the files they live in, with a pointer to the desktop app for
// editing. It is Markdown (the TUI renders assistant-style entries with
// Glamour).
func (v View) Text() string {
	var b strings.Builder
	b.WriteString("## Settings\n\n")
	b.WriteString("To change these, use Settings in the desktop app (the gear button), or edit the files listed at the end.\n\n")
	fmt.Fprintf(&b, "**Institution:** %s (%s)", v.Site.Name, v.Site.Domain)
	if v.Site.DocsURL != "" {
		fmt.Fprintf(&b, ", documentation: `%s`", v.Site.DocsURL)
	}
	b.WriteString("\n\n**Clusters**\n\n")
	if v.ConfigError != "" {
		fmt.Fprintf(&b, "- clusters.yaml can't be read: %s\n", v.ConfigError)
	}
	if len(v.Clusters) == 0 && v.ConfigError == "" {
		b.WriteString("- none yet\n")
	}
	for _, c := range v.Clusters {
		b.WriteString("- " + clusterLine(c) + "\n")
	}
	if len(v.MoreClusters) > 0 {
		b.WriteString("\n**More clusters at your institution (not set up yet)**\n\n")
		for _, m := range v.MoreClusters {
			fmt.Fprintf(&b, "- %s (%s)\n", m.Name, typeName(m.Type))
		}
		b.WriteString("\nTo add one, ask Labmill in the chat (for example \"set up the cluster " + v.MoreClusters[0].Name + "\"), or use Settings in the desktop app.\n")
	}
	b.WriteString("\n**SSH connections**\n\n")
	if v.SSHError != "" {
		fmt.Fprintf(&b, "- the SSH settings can't be read: %s\n", v.SSHError)
	}
	if len(v.Hosts) == 0 && v.SSHError == "" {
		b.WriteString("- none yet\n")
	}
	for _, h := range v.Hosts {
		b.WriteString("- " + hostLine(h, true) + "\n")
	}
	b.WriteString("\n**Commands on the cluster**\n\n")
	if v.App.RunWithoutAsking {
		b.WriteString("- run without asking: on (Labmill runs commands on your cluster account without showing them first; reads of key or token files still ask). To turn it off, set \"runWithoutAsking\": false in the desktop app settings file listed below, or use the desktop app's Settings (a running desktop app may overwrite a hand edit).\n")
	} else {
		b.WriteString("- run without asking: off (Labmill shows each command that isn't a plain read and waits for your OK). To turn it on, set \"runWithoutAsking\": true in the desktop app settings file listed below, or use the desktop app's Settings (a running desktop app may overwrite a hand edit).\n")
	}
	if v.App.ApproveAll {
		b.WriteString("- approve everything automatically: ON (Labmill approves every step for you, without showing it first; reads of key or token files, questions and logins still ask). Turn it off with /autoapprove off, or in the desktop app's main window or Settings.\n")
	} else {
		b.WriteString("- approve everything automatically: off. Turn it on with /autoapprove on, or in the desktop app's main window or Settings.\n")
	}
	if v.App.AutoDownload {
		b.WriteString("- download results automatically: ON (when a job you submitted finishes, Labmill brings its results to your results folder by itself, up to 2 GB and never more than fits on your disk). Turn it off with /autodownload off, or in the desktop app's main window or Settings.\n")
	} else {
		b.WriteString("- download results automatically: off. Turn it on with /autodownload on, or in the desktop app's main window or Settings.\n")
	}
	if v.App.NotifyJobEnd {
		b.WriteString("- notify when a job ends: on (desktop app only). Turn it off in the desktop app's Settings.\n")
	} else {
		b.WriteString("- notify when a job ends: off. Turn it on in the desktop app's Settings.\n")
	}
	if v.App.EmailOnJobEnd {
		b.WriteString("- email when a Slurm job ends: on (new Slurm scripts ask the scheduler to mail you at the end or on failure, at the site's default address). Turn it off in the desktop app's Settings.\n")
	} else {
		b.WriteString("- email when a Slurm job ends: off. Turn it on in the desktop app's Settings.\n")
	}
	b.WriteString("\n**About**\n\n")
	fmt.Fprintf(&b, "- version %s (%s)\n", orNone(v.About.Version), v.About.Platform)
	if v.About.BrokerURL != "" {
		fmt.Fprintf(&b, "- assistant address: `%s`\n", v.About.BrokerURL)
	}
	b.WriteString("\n**Files**\n\n")
	for _, f := range v.About.Files {
		fmt.Fprintf(&b, "- %s: `%s`\n", f.Label, f.Path)
	}
	return b.String()
}

// clusterLine is one cluster in a line: "bigcluster (Slurm) · ssh host
// login · files host files · transfer sftp".
// typeName is a cluster type's display name.
func typeName(t string) string {
	switch t {
	case "slurm":
		return "Slurm"
	}
	return t
}

func clusterLine(c Cluster) string {
	kind := c.Type
	switch c.Type {
	case "slurm":
		kind = "Slurm"
	}
	parts := []string{fmt.Sprintf("%s (%s)", c.Name, kind)}
	add := func(label, v string) {
		if v != "" {
			parts = append(parts, label+" "+v)
		}
	}
	add("ssh host", c.SSHHost)
	add("files host", c.FilesHost)
	tr := c.Transfer
	if tr == "" {
		tr = "sftp"
	}
	add("transfer", tr)
	add("time zone", c.Timezone)
	if c.S3 != nil {
		s3 := "s3://" + c.S3.Bucket
		if c.S3.Prefix != "" {
			s3 += "/" + strings.TrimRight(c.S3.Prefix, "/")
		}
		add("S3", s3)
		add("S3 region", c.S3.Region)
		add("AWS profile", c.S3.Profile)
		add("S3 endpoint", safeURL(c.S3.Endpoint))
		add("encryption", c.S3.SSE)
	}
	return strings.Join(parts, " · ")
}

// hostLine is one SSH connection in a line; withUser adds the user name.
func hostLine(h HostView, withUser bool) string {
	s := h.Alias
	target := h.HostName
	if withUser && h.User != "" {
		target = h.User + "@" + target
	}
	if h.Port != 0 && h.Port != 22 {
		target += fmt.Sprintf(":%d", h.Port)
	}
	s += " → " + target
	if h.ProxyJump != "" {
		jump := h.ProxyJump
		if !withUser {
			jump = jumpWithoutUsers(jump)
		}
		s += " via " + jump
	}
	if h.InLabmill {
		s += " (Labmill's)"
	} else {
		s += " (yours, read-only)"
	}
	if h.LoggedIn {
		s += " · logged in"
	}
	return s
}

// Diagnostics is the plain text of "Copy diagnostics": version, platform,
// broker address, institution, the clusters and connections (no user
// names), and the file locations. It holds no secrets: clusters.yaml has none, user
// names and the s3 profile are left out, and URLs lose any user info and
// query.
func (v View) Diagnostics() string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("Labmill diagnostics")
	line("version: %s (%s)", orNone(v.About.Version), v.About.Platform)
	line("assistant address: %s", orNone(v.About.BrokerURL))
	line("institution: %s (%s)", v.Site.Name, v.Site.Domain)
	if v.ConfigError != "" {
		line("clusters.yaml: can't be read: %s", v.ConfigError)
	}
	line("clusters: %d", len(v.Clusters))
	for _, c := range v.Clusters {
		c.S3 = s3NoProfile(c.S3)
		line("  %s", clusterLine(c))
	}
	if v.SSHError != "" {
		line("ssh settings: can't be read: %s", v.SSHError)
	}
	line("ssh connections: %d", len(v.Hosts))
	for _, h := range v.Hosts {
		line("  %s", hostLine(h, false))
	}
	line("files:")
	for _, f := range v.About.Files {
		line("  %s: %s", f.Label, f.Path)
	}
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// s3NoProfile returns a copy of s without the AWS profile name.
func s3NoProfile(s *S3) *S3 {
	if s == nil {
		return nil
	}
	c := *s
	c.Profile = ""
	c.KMSKeyID = ""
	return &c
}

// jumpWithoutUsers drops the "user@" part of every ProxyJump hop
// ("alice@gw:2222,b" -> "gw:2222,b").
func jumpWithoutUsers(jump string) string {
	hops := strings.Split(jump, ",")
	for i, h := range hops {
		if j := strings.LastIndex(h, "@"); j >= 0 {
			hops[i] = h[j+1:]
		}
	}
	return strings.Join(hops, ",")
}
