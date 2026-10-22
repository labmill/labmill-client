# Site profiles

A site profile holds what is specific to one institution. Each profile is a
folder named by the institution's internet domain (`example.edu`,
`hpc.example.org`, …). Only `generic/`, the profile for an institution without
a profile of its own, is part of this repository. Profiles for real
institutions are kept and shipped apart from the Labmill client.

A profile folder holds:

- `SKILL.md` and `References/*.md`: a **site skill** (named
  `site-<domain with dots as dashes>`, e.g. `site-example-edu`) with the
  institution's specifics: login and transfer hosts, accounts and two-factor
  sign-in, clusters, partitions and GPUs, storage folders, proxy quirks,
  software notes and documentation links.
- `Site.yaml`: the same essentials in machine-readable form for Labmill. Claude
  Code ignores it.
- one folder per cluster: everything specific to that cluster, in
  `<cluster>/References/*.md`, plus a `Cluster.yaml` for Labmill. Facts that
  hold for the whole institution stay at the top. The site skill's `SKILL.md`
  says which cluster folder to read.

The generic skills in `../skills/` (`slurm-user`, `research-computing`) contain
no site facts. They say "see your site skill" wherever a value differs per
site. Use them together with your institution's site skill.

## Naming

- **Names are lowercase:** the institution folders (`example.edu`) and the
  cluster folders (`bigcluster`, and the placeholder `_cluster-template`).
- **Everything else starts with an uppercase letter** and keeps the rest as
  written: folders (`References`, `Evals`, `Diagrams`) and files (`Site.yaml`,
  `Cluster.yaml`, `Slurm.md`, `Hosts-and-access.md`, `Architecture.svg`).
- **Exceptions:** `SKILL.md`, `README.md`, `QUESTIONS.md` and `SOURCE.md` are
  already uppercase, and `embed.go` is Go source and stays lowercase.

A test checks the profiles on disk, so a new profile follows the rule.
Windows and macOS file systems ignore case, so never rely on two names that
differ only in case.

## Using the skills in Claude Code

Claude Code finds skills in `.claude/skills/<skill-name>/` (in a project) or
`~/.claude/skills/<skill-name>/` (for all your projects). The folder name must
match the skill's `name`. Copy the two generic skills and your site skill.
Copy the **whole** site folder, cluster folders included: the site skill
links to their `References/` files.

```bash
# from a checkout of the Labmill client repository
mkdir -p ~/.claude/skills
cp -r skills/slurm-user skills/research-computing ~/.claude/skills/
cp -r sites/generic ~/.claude/skills/site-generic
```

On Windows (PowerShell):

```powershell
New-Item -ItemType Directory -Force "$HOME\.claude\skills" | Out-Null
Copy-Item -Recurse skills\slurm-user, skills\research-computing "$HOME\.claude\skills\"
Copy-Item -Recurse sites\generic "$HOME\.claude\skills\site-generic"
```

If you have a site profile for your institution (for example
`example.edu`), copy that folder as `site-example-edu` instead of
`site-generic`. The generic site skill tells Claude how to find the site
facts by asking you, reading your documentation and querying the cluster.

Start a new Claude Code session afterwards. Ask something like "submit my
training job to the GPU partition" or "why is my Slurm job pending?" and Claude
loads the generic skill and, for site details, the site skill.

## Using them in Labmill

Nothing to install: Labmill embeds the generic skills and the site profiles it
was built with. It asks for your institution at first run and records it in
`~/.config/labmill/clusters.yaml` (`site: <domain>`).

## Adding or fixing a site

See `docs/dev/sites.md` in this repository (the folder layout, `Site.yaml`
and `Cluster.yaml`). Every fact needs a source; mark anything unconfirmed
"(unverified)". `generic/_cluster-template/` is a cluster folder to copy;
Labmill ignores it.
