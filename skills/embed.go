// Package skills holds the skills embedded in the Labmill binary: Anthropic/
// Claude Code-format skill directories (SKILL.md plus references/*.md). The
// skill runtime in internal/skill loads them from FS. See SOURCE.md for where
// they come from.
package skills

import "embed"

// FS contains every embedded skill's SKILL.md and references/*.md, rooted at
// the skill directory names (e.g. "slurm-user/SKILL.md").
//
//go:embed */SKILL.md */references/*.md
var FS embed.FS
