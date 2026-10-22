package app

import (
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"labmill-client/internal/skill"
	"labmill-client/skills"
)

// headingAnchors returns the GitHub-style anchors of md's ATX headings
// ("## Step 1: read the code" → "step-1-read-the-code"): link syntax reduced
// to its text, lowercased, every rune that isn't a letter, digit, space, '-'
// or '_' dropped, spaces turned into '-'; a repeated anchor gets "-1", "-2".
// Headings inside fenced code blocks don't count.
func headingAnchors(md string) map[string]bool {
	out := map[string]bool{}
	seen := map[string]int{}
	fence := ""
	for _, line := range strings.Split(md, "\n") {
		trim := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(trim, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			fence = trim[:3]
			continue
		}
		if !strings.HasPrefix(line, "#") {
			continue
		}
		text := strings.TrimLeft(line, "#")
		if text != "" && text[0] != ' ' && text[0] != '\t' {
			continue // "#hashtag", not a heading
		}
		if len(line)-len(text) > 6 {
			continue
		}
		a := slug(strings.TrimRight(strings.TrimSpace(text), "# "))
		if n, ok := seen[a]; ok {
			seen[a] = n + 1
			a = a + "-" + strconv.Itoa(n+1)
		} else {
			seen[a] = 0
		}
		out[a] = true
	}
	return out
}

var mdLinkRE = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)

// slug is GitHub's heading anchor for text.
func slug(text string) string {
	text = mdLinkRE.ReplaceAllString(text, "$1")
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Which project and which image — always ask, with the AskUserQuestion tool": "which-project-and-which-image--always-ask-with-the-askuserquestion-tool",
		"9. A live CLI session: interactive login and token expiry":                 "9-a-live-cli-session-interactive-login-and-token-expiry",
		"Getting results back: write to `$RESULT_DIR`":                              "getting-results-back-write-to-result_dir",
		"Never point `--output`/`--error` at `/tmp`":                                "never-point---output--error-at-tmp",
		"The payload's tools must already be in the image":                          "the-payloads-tools-must-already-be-in-the-image",
		"See [the docs](https://example.org/x) first":                               "see-the-docs-first",
		"Mounting a cluster folder with --host-path":                                "mounting-a-cluster-folder-with---host-path",
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
	got := headingAnchors("# A\n\n```bash\n# not a heading\n```\n## A\n#hashtag\n### B ###\n")
	for _, a := range []string{"a", "a-1", "b"} {
		if !got[a] {
			t.Errorf("headingAnchors: missing %q in %v", a, got)
		}
	}
	if got["not-a-heading"] || got["hashtag"] || len(got) != 3 {
		t.Errorf("headingAnchors = %v", got)
	}
}

// sectionText returns the embedded file a skillSection names.
func sectionText(t *testing.T, set *skill.Set, s skillSection) string {
	t.Helper()
	k, ok := set.Get(s.Skill)
	if !ok {
		t.Fatalf("skill %q is not embedded", s.Skill)
	}
	if s.File == "SKILL.md" {
		body, err := k.Body()
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	text, err := k.ReadReference(s.File)
	if err != nil {
		t.Fatalf("%s: %v", s.Skill, err)
	}
	return text
}

// Every typed tool with domain logic names the skill sections that describe
// it (so the Go rules and the skill text can't drift apart silently), every
// listed section exists in the embedded skills, and every registered tool is
// either mapped or explicitly without domain logic.
func TestToolSkillSections(t *testing.T) {
	set, err := skill.Load(skills.FS, "embedded")
	if err != nil {
		t.Fatal(err)
	}
	anchors := map[string]map[string]bool{}
	for tool, secs := range toolSkills {
		if len(secs) == 0 {
			t.Errorf("tool %s: no skill sections listed", tool)
		}
		for _, s := range secs {
			key := s.Skill + "/" + s.File
			if anchors[key] == nil {
				anchors[key] = headingAnchors(sectionText(t, set, s))
			}
			if !anchors[key][s.Anchor] {
				t.Errorf("tool %s: skill %s, %s has no section #%s", tool, s.Skill, s.File, s.Anchor)
			}
		}
	}

	registered := map[string]bool{}
	for _, tool := range Tools(&Env{}) {
		registered[tool.Name] = true
		_, mapped := toolSkills[tool.Name]
		none := slices.Contains(noDomainLogic, tool.Name)
		switch {
		case mapped && none:
			t.Errorf("tool %s is both in toolSkills and in noDomainLogic", tool.Name)
		case !mapped && !none:
			t.Errorf("tool %s is in neither toolSkills (internal/app/skillmap.go: the skill sections describing its rules) nor noDomainLogic", tool.Name)
		}
	}
	listed := slices.Clone(noDomainLogic)
	for name := range toolSkills {
		listed = append(listed, name)
	}
	for _, name := range listed {
		if !registered[name] && !slices.Contains(pendingTools, name) {
			t.Errorf("skillmap.go lists %s, which isn't a registered tool (renamed or removed?)", name)
		}
	}
	for _, name := range pendingTools {
		if _, mapped := toolSkills[name]; !mapped && !slices.Contains(noDomainLogic, name) {
			t.Errorf("pending tool %s is in neither toolSkills nor noDomainLogic", name)
		}
	}
}

var linkTargetRE = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// Links between the embedded generic skill files point at files and
// sections that exist, so a renamed heading can't leave a dead link (or a
// dead toolSkills entry) behind.
func TestGenericSkillLinksResolve(t *testing.T) {
	var files []string
	err := fs.WalkDir(skills.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".md") {
			files = append(files, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	anchorsOf := map[string]map[string]bool{}
	read := func(p string) (map[string]bool, bool) {
		if a, ok := anchorsOf[p]; ok {
			return a, true
		}
		data, err := fs.ReadFile(skills.FS, p)
		if err != nil {
			return nil, false
		}
		anchorsOf[p] = headingAnchors(string(data))
		return anchorsOf[p], true
	}
	for _, f := range files {
		data, _ := fs.ReadFile(skills.FS, f)
		for _, m := range linkTargetRE.FindAllStringSubmatch(string(data), -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			file, anchor, _ := strings.Cut(target, "#")
			p := f
			if file != "" {
				p = path.Clean(path.Join(path.Dir(f), file))
			}
			a, ok := read(p)
			if !ok {
				// Files that are not embedded (tools/, evals/, templates)
				// must still exist in the repository.
				if _, err := os.Stat(path.Join("../../skills", p)); err != nil {
					t.Errorf("%s: link %q: no such file", f, target)
				}
				continue
			}
			if anchor != "" && !a[anchor] {
				t.Errorf("%s: link %q: %s has no section #%s", f, target, p, anchor)
			}
		}
	}
}
