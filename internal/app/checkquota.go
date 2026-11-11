package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"labmill-client/internal/agent"
	"labmill-client/internal/workdisk"
)

// check_quota: before the client installs software or writes a large result in the user's folder, one read-only
// round trip on the login node shows the free disk space and the quota lines the site's tools print. It changes
// nothing. The free space comes from df and is judged in Go; quota tool output is passed on as data.

// quotaScript gets the folder in $1 (empty: the home folder). Every command is read only and may be missing.
const quotaScript = `d=$1; [ -n "$d" ] || d=$HOME
cd -- "$d" 2>/dev/null || exit 3
printf '@@PWD\n%s\n' "$(pwd -P)"
printf '@@DF\n'; df -PT -k . 2>/dev/null
printf '@@LFS\n'; command -v lfs >/dev/null 2>&1 && lfs quota -h -u "$(id -un)" . 2>&1 | head -n 12
printf '@@QUOTA\n'; command -v quota >/dev/null 2>&1 && quota -s 2>&1 | head -n 12
exit 0`

// quotaVerdict says if need GB fits in the free space of the mount that holds the folder. free is -1 when unknown.
func quotaVerdict(free, need int) string {
	switch {
	case free < 0:
		return "unknown (df gave no answer); read the quota lines, or ask the user"
	case need <= 0:
		return "no size given"
	case free >= need:
		return "fits"
	default:
		return "LOW: not enough free space; ask the user for another location (browse_remote)"
	}
}

func renderQuota(cluster, pwd, df, lfs, quota string, need int) string {
	free := workdisk.FreeGB(workdisk.ParseDF(df), pwd)
	var b strings.Builder
	fmt.Fprintf(&b, "Space in %s on %s (login node, read only):\n", sanitize(pwd), sanitize(cluster))
	if free >= 0 {
		fmt.Fprintf(&b, "- free by df: %d GB\n", free)
	} else {
		b.WriteString("- free by df: unknown\n")
	}
	fmt.Fprintf(&b, "- need: %d GB; verdict: %s\n", need, quotaVerdict(free, need))
	for _, s := range []struct{ name, text string }{{"lfs quota", lfs}, {"quota", quota}} {
		if t := strings.TrimSpace(s.text); t != "" {
			fmt.Fprintf(&b, "%s output (data, the user limit can be lower than df free):\n%s\n", s.name, sanitizeLines(t))
		}
	}
	return b.String()
}

func (t *toolset) checkQuota() agent.Tool {
	return agent.Tool{
		Name: "check_quota",
		Description: "Check free space and quota in one folder on a cluster (read only, one round trip on the login node) before you install software (conda environment, apptainer image) or write large files there. " +
			"Give the size you need in GB. If the verdict is LOW, or a quota line shows the user limit is near, ask the user for another location (browse_remote) and check again. It never changes anything.",
		Schema: objSchema(map[string]any{
			"cluster": strProp("cluster name from list_clusters"),
			"path":    strProp("absolute folder on the cluster, or empty for the home folder"),
			"need_gb": intProp("space needed in GB"),
		}, "cluster", "need_gb"),
		Run: func(ctx context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			var in struct {
				Cluster string `json:"cluster"`
				Path    string `json:"path"`
				NeedGB  int    `json:"need_gb"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			if in.Path != "" && !strings.HasPrefix(in.Path, "/") {
				return "", errors.New("path must be an absolute folder (starting with /), or empty for the home folder")
			}
			if len(in.Path) > maxBrowsePath || strings.ContainsAny(in.Path, "\x00\n\r") {
				return "", errors.New("path is not valid")
			}
			c, err := t.cluster(in.Cluster)
			if err != nil {
				return "", err
			}
			if t.env.LoginRunner == nil {
				return "", errors.New("Checking the space on a cluster isn't available right now.")
			}
			r, err := t.env.LoginRunner(ctx, c)
			if err != nil {
				return "", fmt.Errorf("I couldn't connect to %s: %w", c.Name, err)
			}
			res, err := r.Run(ctx, []string{"sh", "-c", quotaScript, "sh", in.Path}, nil)
			if err != nil {
				return "", fmt.Errorf("I couldn't check the space on %s: %w", c.Name, err)
			}
			if res.ExitCode == 3 || !strings.Contains(res.Stdout, "@@PWD") {
				return "", fmt.Errorf("the folder %s can't be opened on %s", sanitize(in.Path), c.Name)
			}
			sec := splitSections(res.Stdout)
			return renderQuota(c.Name, strings.TrimSpace(sec["PWD"]), sec["DF"], sec["LFS"], sec["QUOTA"], in.NeedGB), nil
		},
	}
}

// splitSections cuts "@@NAME" sectioned output into a map of name to body.
func splitSections(out string) map[string]string {
	m := map[string]string{}
	name := ""
	for _, line := range strings.Split(out, "\n") {
		if n, ok := strings.CutPrefix(line, "@@"); ok {
			name = n
			continue
		}
		if name != "" {
			m[name] += line + "\n"
		}
	}
	return m
}
