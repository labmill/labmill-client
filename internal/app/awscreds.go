package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"labmill-client/internal/agent"
	"labmill-client/internal/config"
	"labmill-client/internal/textsafe"
)

// M66.1: copy one local AWS profile to the login node as the section
// [labmill-<name>] in ~/.aws/credentials, so jobs there can use S3 with
// AWS_PROFILE=labmill-<name>. The values go from the laptop straight to the
// cluster on the SSH stdin; they are never in argv, the proposal, the
// result, a log or the model's context. The proposal is marked Secret, so
// "Approve everything automatically" never approves it.
//
// M66.2: with enter_keys the user types the keys into a masked dialog
// (Env.SecretPrompt) instead; they take the same path to the cluster.

const (
	s3CredsTimeout = time.Minute
	// awsRemoteSection is the section name prefix on the cluster.
	awsRemoteSection = "labmill-"
)

var awsProfileName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// awsRemoteScript installs the section from stdin as $1's section of
// ~/.aws/credentials (following a symbolic link), keeping every other line,
// through a temp file beside the target and a rename. It is fixed text; only
// $1 varies (validated). It is one line (csh/tcsh reject a newline inside a
// single-quoted word, as in sshx's listKeysScript) and has no "!".
var awsRemoteScript = strings.Join([]string{
	`umask 077`,
	`d="$HOME/.aws"`,
	`mkdir -p "$d" && chmod 700 "$d" || exit 1`,
	`f="$d/credentials"`,
	`if [ -L "$f" ]; then f=$(readlink -f "$f") || exit 1; fi`,
	`new=$(mktemp "$(dirname "$f")/.labmill-new.XXXXXX") || exit 1`,
	`trap "rm -f \"$new\"" EXIT`,
	`trap "exit 1" HUP INT TERM`,
	`if [ -f "$f" ]; then awk -v n="[$1]" '/^\[/ { s = ($0 == n) } s == 0 { print }' "$f" > "$new" || exit 1; fi`,
	`cat >> "$new" || exit 1`,
	`chmod 600 "$new" && mv -f "$new" "$f"`,
}, "; ")

func awsRemoteArgv(name string) []string {
	return []string{"sh", "-c", awsRemoteScript, "labmill-s3", awsRemoteSection + name}
}

// awsFiles are the local config and credentials files (the SDK's rules).
func awsFiles() (confFile, credFile string) {
	confFile, credFile = os.Getenv("AWS_CONFIG_FILE"), os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	if home, err := os.UserHomeDir(); err == nil {
		if confFile == "" {
			confFile = filepath.Join(home, ".aws", "config")
		}
		if credFile == "" {
			credFile = filepath.Join(home, ".aws", "credentials")
		}
	}
	return
}

// awsProfile is a profile's name and kind; never a value.
type awsProfile struct{ Name, Kind string }

// readAWSSections returns section name -> keys present (never the values).
// In the config file, sections are [default] and [profile x]; others
// ([sso-session x]) are skipped.
func readAWSSections(file string, isConfig bool) map[string]map[string]bool {
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]map[string]bool{}
	var cur map[string]bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			name := strings.TrimSpace(line[1 : len(line)-1])
			cur = nil
			if isConfig && name != "default" {
				rest, ok := strings.CutPrefix(name, "profile ")
				if !ok {
					continue
				}
				name = strings.TrimSpace(rest)
			}
			if out[name] == nil {
				out[name] = map[string]bool{}
			}
			cur = out[name]
		case cur != nil && line != "" && line[0] != '#' && line[0] != ';':
			if k, _, ok := strings.Cut(line, "="); ok {
				cur[strings.ToLower(strings.TrimSpace(k))] = true
			}
		}
	}
	return out
}

// listAWSProfiles lists the local profiles with their kind: static (access
// keys), sso, role, process, web-identity or other.
func listAWSProfiles() []awsProfile {
	confFile, credFile := awsFiles()
	keys := map[string]map[string]bool{}
	for _, src := range []struct {
		file   string
		isConf bool
	}{{confFile, true}, {credFile, false}} {
		for n, ks := range readAWSSections(src.file, src.isConf) {
			if keys[n] == nil {
				keys[n] = map[string]bool{}
			}
			for k := range ks {
				keys[n][k] = true
			}
		}
	}
	var out []awsProfile
	for n, k := range keys {
		if !awsProfileName.MatchString(n) {
			continue
		}
		kind := "other"
		switch {
		case k["sso_session"] || k["sso_start_url"] || k["sso_account_id"]:
			kind = "sso"
		case k["credential_process"]:
			kind = "process"
		case k["role_arn"] && k["web_identity_token_file"]:
			kind = "web-identity"
		case k["role_arn"]:
			kind = "role"
		case k["aws_access_key_id"]:
			kind = "static"
		}
		out = append(out, awsProfile{n, kind})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// awsSection resolves the profile with the SDK and renders the section.
// expires is zero for keys that do not expire.
func awsSection(ctx context.Context, name string) (section string, expires time.Time, err error) {
	c, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithSharedConfigProfile(name))
	if err != nil {
		return "", time.Time{}, err
	}
	cr, err := c.Credentials.Retrieve(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s%s]\n", awsRemoteSection, name)
	if cr.CanExpire {
		expires = cr.Expires.UTC()
		fmt.Fprintf(&b, "# expires %s\n", expires.Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "aws_access_key_id = %s\naws_secret_access_key = %s\n", cr.AccessKeyID, cr.SecretAccessKey)
	if cr.SessionToken != "" {
		fmt.Fprintf(&b, "aws_session_token = %s\n", cr.SessionToken)
	}
	return b.String(), expires, nil
}

type s3CredsPlan struct {
	cluster config.Cluster
	name    string
	kind    string
	typed   bool // M66.2: the keys come from the masked dialog; name is chosen there
}

// s3CredsPlan checks the input. It returns a plan, or a message for the
// model (a listing or a refusal) when nothing is to be copied.
func (t *toolset) s3CredsPlan(input json.RawMessage) (*s3CredsPlan, string, error) {
	var in struct {
		Cluster   string `json:"cluster"`
		Profile   string `json:"profile"`
		EnterKeys bool   `json:"enter_keys"`
	}
	if err := decode(input, &in); err != nil {
		return nil, "", err
	}
	if t.env.Site.NoS3Credentials {
		return nil, "Your institution's profile turns off copying S3 credentials to the cluster. Jobs there can't use your S3 keys; stage data with upload_data instead.", nil
	}
	profiles := listAWSProfiles()
	if in.EnterKeys {
		if t.env.SecretPrompt == nil {
			return nil, "Typing keys into a dialog isn't available here. Tell the user to run `aws configure` themselves. Never ask for keys in the chat.", nil
		}
		cl, err := t.cluster(in.Cluster)
		if err != nil {
			return nil, "", err
		}
		return &s3CredsPlan{cluster: cl, kind: "typed by the user", typed: true}, "", nil
	}
	if in.Profile == "" || in.Cluster == "" {
		var b strings.Builder
		b.WriteString("Local AWS profiles (names and kinds only; pass cluster and profile to copy one):\n")
		for _, p := range profiles {
			fmt.Fprintf(&b, "- %s (%s)\n", p.Name, p.Kind)
		}
		if len(profiles) == 0 {
			b.WriteString("(none). Tell the user to create a profile in their own terminal with `aws configure` (or `aws configure sso`), then ask again. Never ask the user to type keys into the chat.\n")
		}
		return nil, b.String(), nil
	}
	cl, err := t.cluster(in.Cluster)
	if err != nil {
		return nil, "", err
	}
	if !awsProfileName.MatchString(in.Profile) {
		return nil, "", errors.New("The profile name may only have letters, digits, dot, underscore and dash.")
	}
	for _, p := range profiles {
		if p.Name == in.Profile {
			return &s3CredsPlan{cluster: cl, name: p.Name, kind: p.Kind}, "", nil
		}
	}
	return nil, "", fmt.Errorf("There's no local AWS profile %q. Call this tool without a profile to list them.", in.Profile)
}

// proposal never holds a value. The summary has no expiry, so Run can
// compare it with the approved one after resolving the credentials again.
func (p *s3CredsPlan) proposal(expires time.Time) agent.Proposal {
	if p.typed {
		return agent.Proposal{
			Tool:    "s3_credentials_to_cluster",
			Summary: fmt.Sprintf("Type AWS keys into a private dialog and save them on %s", sanitize(p.cluster.Name)),
			Details: fmt.Sprintf("Cluster: %s\nFile: ~/.aws/credentials (folder mode 700, file mode 600)\nSection: [%s<name you choose in the dialog>] (other sections stay as they are)\nA dialog asks you for the keys. They go from this computer straight to the cluster. Claude never sees them. Anyone who can read your files on the cluster can use them.", sanitize(p.cluster.Name), awsRemoteSection),
			Secret:  true,
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Cluster: %s\nFile: ~/.aws/credentials (folder mode 700, file mode 600)\nSection: [%s%s] (other sections stay as they are)\nKind: %s\n", sanitize(p.cluster.Name), awsRemoteSection, p.name, p.kind)
	if !expires.IsZero() {
		fmt.Fprintf(&b, "Expires: %s (temporary credentials)\n", expires.Format(time.RFC3339))
	}
	b.WriteString("The keys go from this computer straight to the cluster. Claude never sees them. Anyone who can read your files on the cluster can use them.")
	return agent.Proposal{
		Tool:    "s3_credentials_to_cluster",
		Summary: fmt.Sprintf("Copy AWS profile %s to %s as %s%s", p.name, sanitize(p.cluster.Name), awsRemoteSection, p.name),
		Details: b.String(),
		Secret:  true, // never approved by "Approve everything automatically"
	}
}

func (t *toolset) s3CredentialsToCluster() agent.Tool {
	return agent.Tool{
		Name: "s3_credentials_to_cluster",
		Description: "Copy ONE local AWS profile to a Slurm cluster's login node so jobs there can read and write S3 themselves. " +
			"Call it with no profile to list the local profiles (names and kinds only: static, sso, role, process). " +
			"With cluster and profile it asks the user (always, also with automatic approval) and writes the section [labmill-<profile>] in ~/.aws/credentials there (modes 700/600, other sections untouched). " +
			"SSO and role profiles are copied as temporary credentials with an expiry; if the sign-in expired, tell the user to run `aws sso login --profile <name>` in a terminal. You never see the keys. " +
			"Afterwards put AWS_PROFILE=labmill-<profile> in the job script. If there is no profile, call it with cluster and enter_keys true: the user then types the keys into a masked dialog that you never see. Never ask for keys in the chat.",
		Schema: objSchema(map[string]any{
			"cluster":    strProp("Slurm cluster name"),
			"profile":    strProp("local AWS profile name; leave empty to list the profiles"),
			"enter_keys": map[string]any{"type": "boolean", "description": "true when there is no local profile: the user types the keys into a masked dialog"},
		}),
		NeedsApproval: true,
		Propose: func(input json.RawMessage) (agent.Proposal, error) {
			p, _, err := t.s3CredsPlan(input)
			if err != nil {
				return agent.Proposal{}, err
			}
			if p == nil {
				return agent.Proposal{}, agent.ErrNoApproval // a listing or a refusal: Run says it
			}
			ctx, cancel := context.WithTimeout(context.Background(), s3CredsTimeout)
			defer cancel()
			if p.typed {
				return p.proposal(time.Time{}), nil
			}
			_, exp, err := awsSection(ctx, p.name)
			if err != nil {
				return agent.Proposal{}, p.resolveErr(err)
			}
			return p.proposal(exp), nil
		},
		Run: func(ctx context.Context, input json.RawMessage, d agent.Decision) (string, error) {
			p, msg, err := t.s3CredsPlan(input)
			if err != nil || p == nil {
				return msg, err
			}
			if !d.Approved {
				return "", errors.New("Copying credentials needs the user's approval, which it didn't get.")
			}
			if t.env.LoginRunner == nil {
				return "", errors.New("Running commands on the cluster isn't available right now.")
			}
			var section string
			var exp time.Time
			if p.typed {
				section, p.name, err = t.askAWSKeys(ctx, "Name for this profile (empty: manual)")
				if err != nil {
					return "", err
				}
			} else if section, exp, err = awsSection(ctx, p.name); err != nil {
				return "", p.resolveErr(err)
			}
			if d.Proposal.Summary != p.proposal(exp).Summary {
				return "", errChanged
			}
			r, err := t.env.LoginRunner(ctx, p.cluster)
			if err != nil {
				return "", fmt.Errorf("I couldn't connect to %s: %w", p.cluster.Name, err)
			}
			cctx, cancel := context.WithTimeout(ctx, s3CredsTimeout+remoteSlack)
			defer cancel()
			res, err := r.Run(cctx, timeoutWire(s3CredsTimeout, awsRemoteArgv(p.name)), strings.NewReader(section))
			if err != nil {
				return "", fmt.Errorf("I couldn't write the credentials on %s: %w", p.cluster.Name, err)
			}
			if res.ExitCode != 0 {
				msg := []rune(textsafe.Clean(res.Stderr)) // paths only, never keys
				if len(msg) > 300 {
					msg = msg[:300]
				}
				return "", fmt.Errorf("Writing the credentials on %s failed (exit %d): %s", p.cluster.Name, res.ExitCode, strings.TrimSpace(string(msg)))
			}
			out := fmt.Sprintf("Copied profile %s to %s as [%s%s] in ~/.aws/credentials. In the job script set AWS_PROFILE=%s%s (and AWS_DEFAULT_REGION or --region; the credentials file holds no region).", p.name, sanitize(p.cluster.Name), awsRemoteSection, p.name, awsRemoteSection, p.name)
			if !exp.IsZero() {
				out += fmt.Sprintf(" These are temporary credentials and expire at %s; a job running longer needs a fresh copy.", exp.Format(time.RFC3339))
			}
			return out, nil
		},
	}
}

// resolveErr is a fixed message: the SDK's errors can hold the raw output of
// a credential_process (the keys), so their text never reaches the model.
func (p *s3CredsPlan) resolveErr(error) error {
	hint := ""
	if p.kind == "sso" {
		hint = fmt.Sprintf(" If the sign-in expired, the user must run `aws sso login --profile %s` in a terminal, then ask again.", p.name)
	}
	return fmt.Errorf("I couldn't get credentials for the AWS profile %s (%s).%s", p.name, p.kind, hint)
}

var awsKeyID = regexp.MustCompile(`^(AKIA|ASIA)[A-Z0-9]{16}$`)

// askAWSKeys runs the masked dialog and returns the rendered section and its
// profile name. Errors never hold a typed value.
func (t *toolset) askAWSKeys(ctx context.Context, nameLabel string) (section, name string, err error) {
	a, err := t.env.SecretPrompt(ctx, "AWS", []PromptField{ // the dialogs show "Sign in to AWS"
		{Label: "AWS access key ID"},
		{Label: "AWS secret access key", Secret: true},
		{Label: "Session token (optional, leave empty for none)", Secret: true},
		{Label: nameLabel},
	})
	if err != nil {
		return "", "", errors.New("Cancelled: the user closed the dialog and nothing was written.")
	}
	if len(a) != 4 {
		return "", "", errors.New("The dialog gave no answer.")
	}
	id, secret, token, name := strings.TrimSpace(a[0]), strings.TrimSpace(a[1]), strings.TrimSpace(a[2]), strings.TrimSpace(a[3])
	if name == "" {
		name = "manual"
	}
	switch {
	case !awsKeyID.MatchString(id):
		err = errors.New("The access key ID doesn't look right (it starts with AKIA or ASIA and has 20 characters). Nothing was written.")
	case secret == "":
		err = errors.New("The secret access key was empty. Nothing was written.")
	case !awsProfileName.MatchString(name) || len(name) > 32 || name == secret:
		// The name goes to the model: a long one may be a key typed in the wrong field.
		err = errors.New("The profile name may only have letters, digits, dot, underscore and dash, at most 32 characters. Nothing was written.")
	}
	if err != nil {
		return "", "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s%s]\naws_access_key_id = %s\naws_secret_access_key = %s\n", awsRemoteSection, name, id, secret)
	if token != "" {
		fmt.Fprintf(&b, "aws_session_token = %s\n", token)
	}
	return b.String(), name, nil
}
