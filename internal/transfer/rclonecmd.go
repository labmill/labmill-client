package transfer

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	rfs "github.com/rclone/rclone/fs"
)

// RcloneCommand returns the rclone-compatible parameters of the transfer that is
// the same as Upload (upload true; rels are the manifest's paths below
// localRoot) or Download (upload false; rels are the paths below the remote
// folder, localRoot is the local destination folder). Every line starts with
// "rclone-lib: ": Labmill runs rclone as a built-in library and starts no rclone
// process. Replacing that prefix with "rclone" gives a command for a
// stand-alone rclone. Downloads give one line per path, and a
// folder's contents land directly in localRoot (Labmill keeps the folder name:
// for a nested path add it to the local destination). The text is quoted for
// the user's shell (POSIX on Unix, PowerShell on Windows) and never holds
// credentials.
func (r Remote) RcloneCommand(localRoot string, rels []string, upload bool) string {
	return r.rcloneCommand(runtime.GOOS, localRoot, rels, upload)
}

func (r Remote) rcloneCommand(goos, localRoot string, rels []string, upload bool) string {
	ssh := r.SSHCommand
	if len(ssh) == 0 {
		ssh = []string{"labmill", "ssh-exec", "<alias>"}
	}
	// --sftp-ssh takes rclone's space-separated list (CSV quoting inside).
	opts := []string{"--sftp-ssh", rfs.SpaceSepList(ssh).String(), "--sftp-shell-type", "none", "--sftp-disable-hashcheck"}
	return buildRclone(goos, localRoot, rels, upload, opts, ":sftp:", r.Dir)
}

// RcloneCommand is Remote.RcloneCommand for an S3 location: the backend is
// given as a connection string (provider, env_auth, profile, region,
// endpoint, encryption), so the command needs no config file, and the keys come
// from the AWS credential chain, never from the command.
func (r S3Remote) RcloneCommand(localRoot string, rels []string, upload bool) string {
	// Show the region the transfer really uses (the profile's), without network.
	r.Region = r.resolveRegion(context.Background())
	return r.rcloneCommand(runtime.GOOS, localRoot, rels, upload)
}

func (r S3Remote) rcloneCommand(goos, localRoot string, rels []string, upload bool) string {
	provider := "AWS"
	if r.Endpoint != "" {
		provider = "Other"
	}
	kv := [][2]string{{"provider", provider}, {"env_auth", "true"}, {"profile", r.Profile}, {"region", r.Region}, {"endpoint", r.Endpoint}}
	for _, k := range []string{"server_side_encryption", "sse_kms_key_id"} {
		if v := r.sseOptions()[k]; v != "" {
			kv = append(kv, [2]string{k, v})
		}
	}
	prefix := ":s3"
	for _, p := range kv {
		if p[1] != "" {
			prefix += "," + p[0] + "=" + connValue(p[1])
		}
	}
	prefix += ":"
	parts := []string{r.Bucket}
	for _, p := range []string{r.Prefix, r.Dir} {
		if p = strings.Trim(p, "/"); p != "" {
			parts = append(parts, p)
		}
	}
	return buildRclone(goos, localRoot, rels, upload, []string{"--s3-no-check-bucket"}, prefix, strings.Join(parts, "/"))
}

// buildRclone assembles the command(s). remotePrefix is the ":backend,opts:"
// part; remoteDir is appended to it.
func buildRclone(goos, local string, rels []string, upload bool, opts []string, remotePrefix, remoteDir string) string {
	q := func(s string) string { return shellQuote(goos, cleanControl(s)) }
	var qo []string
	for _, o := range opts {
		qo = append(qo, q(o))
	}
	flags := strings.Join(qo, " ")
	if upload {
		return fmt.Sprintf("rclone-lib: copy --files-from-raw files.txt --no-traverse %s %s %s  # files.txt lists the %d approved files",
			flags, q(local), q(remotePrefix+remoteDir), len(rels))
	}
	lines := make([]string, len(rels))
	for i, rel := range rels {
		src := remotePrefix + path.Join(remoteDir, filepath.ToSlash(rel))
		lines[i] = fmt.Sprintf("rclone-lib: copy %s %s %s", flags, q(src), q(local))
	}
	return strings.Join(lines, "\n")
}

// connValue quotes a value in an rclone connection string: plain values stay
// as they are, others go in single quotes (a quote is doubled).
func connValue(v string) string {
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_-./", c)) {
			return "'" + strings.ReplaceAll(v, "'", "''") + "'"
		}
	}
	return v
}

// cleanControl replaces control characters with '?', so a path holding a
// newline can't fake an extra rclone-lib line.
func cleanControl(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

// shellQuote quotes s for one shell word: PowerShell single quotes (a quote is
// doubled) on windows, POSIX single quotes ('\” for a quote) elsewhere.
// Words made only of safe characters stay unquoted.
func shellQuote(goos, s string) string {
	safe := s != ""
	chars := "_@%+=:,./-"
	if goos == "windows" {
		// In PowerShell an unquoted a,b is an array and a leading @ splats.
		chars = "_%+=:./-@"
		safe = safe && s[0] != '@'
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune(chars, c)) {
			safe = false
			break
		}
	}
	switch {
	case safe:
		return s
	case goos == "windows":
		// PowerShell treats the smart quotes as single quotes too.
		return "'" + strings.NewReplacer("'", "''", "\u2018", "\u2018\u2018", "\u2019", "\u2019\u2019", "\u201a", "\u201a\u201a", "\u201b", "\u201b\u201b").Replace(s) + "'"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
