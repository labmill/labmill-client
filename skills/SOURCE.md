# Generic skills

These skills come from **yeetl** (copied on 2026-10-22; only the Slurm and
research-computing skills). From then on Labmill is the master copy of the generic Slurm skills;
nothing is synced back automatically.

- `slurm-user/` started in yeetl as a sync of an upstream Claude Code skill
  set, under the MIT license in `LICENSE.upstream`, and was extended in yeetl.
- `research-computing/` was written in yeetl.

They are **generic**: every site-specific fact (host names, domains, cluster
names, partitions, GPU and driver versions, storage paths, proxy addresses,
accounts and two-factor details) lives in a site skill under
`../sites/<domain>/` (see `../sites/README.md` and `docs/dev/sites.md`). Where
a value differs per site, the skills say "see your site skill" (a skill named
`site-<institution domain>`).

The skills are meant to be used unmodified in both places: in Claude Code
(copy them into `.claude/skills/` together with a site skill) and in Labmill,
which embeds each skill's `SKILL.md` and `references/*.md` (see `embed.go`)
and maps the shell steps to its typed tools in its own system prompt
(`internal/app/skillmap.go`; `TestToolSkillSections` and
`TestGenericSkillLinksResolve` fail when a section or a link target
disappears).

`internal/site/leak_test.go`'s `TestGenericSkillsHaveNoSiteFacts` fails if a
site-specific term creeps back into a skill directory here.
