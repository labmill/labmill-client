# Sites: adding an institution

Labmill keeps everything that differs between institutions in **site profiles**
under `sites/<domain>/`. Only `sites/generic/` is part of this repository; profiles
for real institutions are kept and shipped apart from the client. The skills
in `skills/` (`slurm-user`, `research-computing`) are generic and say "see your site skill" wherever a value differs per site.
One set of skills serves Claude Code and Labmill; neither forks them.

## What a site folder contains

Facts that hold for the whole institution sit at the top of `sites/<domain>/`.
Everything specific to one cluster sits in a **cluster folder** named after
it, so Claude reads only the cluster it needs:

```
sites/example.edu/
  Site.yaml             institution-wide values for Labmill (hosts, 2FA)
  SKILL.md              the site skill, name: site-example-edu; indexes the cluster folders
  References/*.md       institution-wide facts: accounts, two-factor sign-in, the
                        shared login and transfer hosts, campus proxy, general
                        storage rules, documentation links
  QUESTIONS.md          open questions and unverified items (not embedded)
  SOURCE.md             optional: where upstream skill text came from (domain root only, not embedded)
  Evals/*.json          optional site-specific skill evals (not embedded)
  Diagrams/*.svg        optional architecture diagrams, plus a Diagrams/README.md
                        describing them (not embedded, not a skill reference)
  bigiron/              a cluster folder (here: a Slurm cluster)
    Cluster.yaml        the clusters.yaml template entry plus cluster hints
    References/*.md     partitions, GPUs, limits, modules, cluster paths
    QUESTIONS.md        optional, open questions about this cluster
    Evals/*.json        optional
    Diagrams/*.svg      optional (and Diagrams/README.md)
```

A cluster folder's name is
lowercase letters, digits and `-` (at most 63 characters) and can't be
`references`, `evals`, `diagrams` or `tools`. Labmill ignores folders whose name starts
with `_` (`sites/generic/_cluster-template/` is a template to copy).

- **The folder name** is the institution's internet domain, lowercase
  (`example.edu`, `example.org`).
- **`SKILL.md`** has the usual frontmatter. `name` is `site-` plus the domain
  with dots replaced by dashes (`site-example-edu`). The `description` starts
  with "<Institution> (<short name>) specifics for HPC/Slurm (<cluster names>):
  …" and lists what the references cover, so Claude loads it
  whenever a site fact is needed. The body is short: an index of the cluster
  folders ("for Slurm on bigiron read `bigiron/References/…`"), a "Values at
  a glance" table and a list of the references with their paths
  (`References/Access.md`, `bigiron/References/Slurm.md`).
- **`References/*.md`** hold the institution-wide facts (login and transfer
  hosts, off-campus access, accounts and two-factor sign-in, storage
  services, proxy quirks, documentation links), and
  **`<cluster>/References/*.md`** the facts of one cluster (partitions and GPU
  types, modules, driver/CUDA version, cluster paths). Keep the
  shell commands Claude Code users need. Links between reference files are
  relative (`../bigiron/References/Slurm.md` from `References/Access.md`);
  write the link text as the path from the skill folder
  (`[bigiron/References/Slurm.md](../bigiron/References/Slurm.md)`), which
  is also what Labmill's `read_reference` accepts.
- **`Site.yaml`** and **`Cluster.yaml`** are described below. Claude Code
  ignores them.
- **`Diagrams/`** holds architecture drawings as SVG (plus a `README.md`
  describing them). They are neither embedded in Labmill nor served as skill
  references.

`sites/generic/` is the profile for "My institution isn't listed". Its skill,
`site-generic`, tells Claude how to find site facts without a profile (ask the
user, read their documentation, query the cluster). It stays flat (no cluster
folders); its `_cluster-template/` folder is only a template to copy, and it
gives `sites/embed.go`'s cluster-folder patterns a file to match in a build
with no other profiles.

## Rules for facts

- Every fact has a source: an official documentation URL, the upstream skill
  it came from, or a confirmed answer from the site. Put sources at the top of
  each reference file or next to the fact.
- Anything not confirmed by an official page is marked **(unverified)**.
- Never guess a host name, project, partition or path. Leave it out (and the
  `Site.yaml` value empty) and add the question to the site's own
  `sites/<domain>/QUESTIONS.md`.
  **Never log site questions or site facts in public files** (`docs/`,
  commit messages).
- **No site term goes into public code or public files**: not into `skills/`,
  `sites/generic/`, Go code or tests (tests use an `example.edu` fixture
  profile parsed with `site.Parse`/`site.Load`). `internal/site`'s
  `TestGenericSkillsHaveNoSiteFacts` builds its forbidden terms from every
  embedded profile (domain, name, hostnames and their parent domains, aliases,
  cluster names and cluster folder names, URL hosts, plus the
  `forbidden_terms` of `Site.yaml` and of every `Cluster.yaml`) and scans
  `skills/` and `sites/generic/`, so the test itself names no site. Add the
  terms it can't derive (project ids, bucket and asset names, short host
  names, version strings) to `forbidden_terms`: institution-wide terms in
  `Site.yaml`, a cluster's own terms in its `Cluster.yaml`.

## Site.yaml

```yaml
domain: example.edu            # must equal the folder name ("generic" for the generic profile)
default: false                 # optional; true makes this the preselected choice in the first-run form (at most one profile; never generic)
name: Example University       # shown in the first-run choice
skill: site-example-edu        # must be "site-" + domain with dots as dashes
docs_url: https://hpc.example.edu/docs      # optional, https
broker_url: ""                 # optional Claude broker for this site; empty = call Anthropic directly with ANTHROPIC_API_KEY
broker:
  issuers: []                  # optional: token issuers a broker maps to this profile; no issuer may be listed by two sites
two_factor:
  help: >-                     # shown when two-factor sign-in doesn't complete
    Example uses Duo. Enroll at https://it.example.edu/duo, then try again.
  url: https://it.example.edu/duo           # optional, https
  name: Duo                    # optional product name for prompts ("approve a Duo sign-in request"); omit for generic wording; plain text, <= 40 chars
s3_credentials_on_cluster: true  # optional; false turns off s3_credentials_to_cluster (copying one AWS profile to the login node); omitted = allowed
hosts:                         # SSH host templates for ~/.ssh/config.d/labmill
  - alias: ex-gw               # lowercase letters, digits, dashes
    hostname: gw.example.edu
    role: jump                 # login | transfer | jump
  - alias: ex-login
    hostname: login.example.edu
    role: login
    proxy_jump: ex-gw          # optional: another alias in this list
    port: 22                   # optional
    auth: password             # optional: password (default; keyboard-interactive + 2FA) | key (SSH keys only)
key_upload:                    # optional: where a new public key is registered, for hosts with auth: key
  url: https://reg.example.edu/keys          # https
  instruction: Open Profile, SSH keys, Add key, and paste the public key.   # one line, <= 300 chars
forbidden_terms:               # optional: extra terms that must never appear in the
  - "1234567"                  #   public skills (project ids, bucket names, short
  - ex-scratch                 #   host names, driver versions); at least 3 characters
```

Notes on some keys:

- **`default`**: `site.Default()` returns the one profile with `default: true`, or generic when none sets it. A `clusters.yaml` without `site:` means that profile, and first-run setup preselects it. Two profiles marked default, or `default` on generic, is an error.
- **`broker_url`**: when set (or when `LABMILL_BROKER_URL` is set), model calls go to that broker with the token from `LABMILL_BROKER_TOKEN`. When empty, the client calls the Anthropic Messages API with the user's own `ANTHROPIC_API_KEY`.
- **`two_factor.name`**: the product name used in prompts ("approve a Duo sign-in request"). Omit it and the login dialog uses generic wording; Labmill's code names no product.
- **`hosts[].auth` and `key_upload`**: a host with `auth: key` accepts SSH keys only. The first login there runs Labmill's key setup (see [SSH](architecture.md#key-only-ssh-hosts)): it looks for a usable key (a configured `IdentityFile`, the default `~/.ssh/id_ed25519`, `id_ecdsa`, `id_rsa`, or an agent key) and otherwise creates an ed25519 key, shows the public key with `key_upload.instruction` and `key_upload.url`, waits for the user's confirmation and writes `IdentityFile` into `~/.ssh/config.d/labmill`. `key_upload` is site-wide and optional; without it the dialog says to upload the key as the site's documentation describes. The values belong in the site skill's access reference too.
- **Reserved folder names**: `References`, `Evals`, `Diagrams` and `Tools` are never cluster folders.

Unknown keys are an error, so typos surface. `site.Parse` checks the domain,
the skill name, that URLs are https, that host aliases are unique and valid,
that roles are known and that `proxy_jump` names a host in the file.

For older profiles `Site.yaml` still accepts `clusters:` (the same entries as
`Cluster.yaml`, listed before the folder templates). New profiles put them in
the cluster folders.

## Cluster.yaml

```yaml
# sites/example.edu/bigiron/Cluster.yaml
name: bigiron
type: slurm
ssh_host: ex-login             # an alias from Site.yaml hosts
files_host: ex-files           # optional transfer-host alias
transfer: sftp
order: 0                       # optional: sort key among the site's clusters (below)
preemptible:                   # optional, Slurm only: the preemptible resource, if any
  partition: preempt           #   partition and/or qos (at least one); names [A-Za-z0-9._-]
  qos: ""                      #   e.g. lowprio
  requeue: true                #   jobs may be submitted with --requeue
  fallback_partition: ""       #   non-preemptible partition after the failed attempts; "" = the default
local_scratch:                 # optional, Slurm only: node-local disk on the compute nodes
  path: /tmp                   #   absolute folder or $VAR[/folder]; path or env_var is required
  env_var: TMPDIR              #   the variable the job finds its folder in; wins over path
  size_gb: 400                 #   0/omitted = unknown
global_scratch:                # optional, Slurm only: shared scratch on the compute nodes
  path: /scratch/<group>       #   absolute, clean; <placeholder> = the user's own part
  purge_days: 90               #   0/omitted = unknown
software_dir: /software/mygroup # optional, Slurm only: absolute clean folder for reusable environments (venv, conda, images); omitted = $HOME/.labmill/envs; not a purged scratch
```

`site.ParseCluster` rejects unknown keys and checks the template like a
`clusters.yaml` entry; `site.Load` then checks that cluster names are unique
across the site and that `ssh_host` and `files_host` name hosts in
`Site.yaml`. `preemptible`, `local_scratch`, `global_scratch` and
`software_dir` (`rules.SlurmHints`, checked by `SlurmHints.Validate`;
`software_dir` is where `draft_job`'s `environment` builds venvs, conda
environments and SIF images) are only allowed on a Slurm cluster; unknown
facts are left out, never guessed. `Site.SlurmHintsFor(name)` returns them;
`cluster_info` states them in plain words and `draft_job` applies them
(`scratch`, `preemptible`; see architecture.md). A `Cluster.yaml` with only
comments (or none at all) declares no template: the folder then holds only
references, for example while the login host is unconfirmed. Each file holds
exactly one YAML document: a second one (after `---`) is an error rather than
silently ignored (the same for `Site.yaml`).

The cluster templates go to `clusters.yaml` in this order: `Site.yaml`'s own
`clusters:` (older profiles), then the cluster folders. `Cluster.yaml`'s
optional `order:` (an integer, default 0, negative allowed) sorts lower first,
and equal values go by folder name. Use it to keep a site's main cluster first
(the order of the prompt's configured clusters, `list_clusters` and the status
line) when a smaller cluster's folder name sorts earlier.

Every value except `domain`, `name` and `skill` may be empty:

- **No login host:** first-run setup asks the user for the login host and
  builds the hosts and clusters from it, as for the generic profile.
- **No broker:** Labmill calls Anthropic directly with `ANTHROPIC_API_KEY`;
  without that key each chat turn explains that the assistant isn't connected.
- **No two-factor help:** the login dialog shows its generic text.

## Naming

- **Names are lowercase:** the institution folders (`example.edu`) and the
  cluster folders (`bigcluster`, and the placeholder `_cluster-template`).
- **Everything else starts with an uppercase letter** and keeps the rest as
  written: folders (`References`, `Evals`, `Diagrams`) and files (`Site.yaml`,
  `Cluster.yaml`, `Slurm.md`, `Hosts-and-access.md`, `Architecture.svg`).
- **Exceptions:** `SKILL.md`, `README.md`, `QUESTIONS.md` and `SOURCE.md` are
  already uppercase, and `embed.go` is Go source and stays lowercase.

`TestSitesNaming` in `internal/site` checks the profiles on disk, so a new
profile follows the rule.
Windows and macOS file systems ignore case, so never rely on two names that
differ only in case.

## How Labmill uses a site

- `sites/embed.go` embeds every folder's `Site.yaml`, `SKILL.md` and
  `References/*.md`, and every cluster folder's `Cluster.yaml` and
  `References/*.md` (package `sites`). `QUESTIONS.md`, `Evals/` and
  `Diagrams/` stay out of the binary.
- `internal/site` parses them: `site.Get(domain)`, `site.List()` (the default
  first), `site.Choices()` (plus generic), `site.Default()` (the profile with `default: true`, or generic
  when none sets it), `site.ForConfig(cfg)` and
  `site.Current()`. `LABMILL_BROKER_URL` overrides the broker URL.
- First-run setup (`launch.Build` with the front end's form) offers
  `site.Choices()`, then `launch.WriteSetup` writes the host templates to
  `~/.ssh/config.d/labmill` (reusing the user's own aliases, rewriting
  `ProxyJump` to the final alias) and saves `site: <domain>` with the clusters
  to `clusters.yaml`. A config without `site` means the default site; an
  unknown site falls back to generic with a startup warning.
- **Other institutions.** A session's home institution is the
  top-level `site:` of `clusters.yaml`. The other embedded profiles are known:
  `site.Others(home)` lists them, `site.SkillsFSSession` adds all their site
  skills to the home ones (a broken skill of another institution is a startup
  warning, only the home one is fatal), and the prompt names each with its
  clusters and skill. Work goes to the home institution unless the user
  explicitly names another; then `add_cluster` copies the cluster's
  `Cluster.yaml` template into `clusters.yaml` with a per-cluster
  `site: <domain>` (empty means the top-level site) and writes the SSH hosts
  it needs. A cluster with its own `site:` takes its hints, host_path, 2FA
  help and key-upload text from that institution's profile. Model calls stay
  the home institution's.
- `Site.SkillsFS(skills.FS)` adds the site skill (`SKILL.md`,
  `References/*.md`, `<cluster>/References/*.md`) to the generic skills under
  its own name, so `load_skill` can load it. The skill runtime lists a
  cluster folder's references as `<cluster>/<file>.md` (e.g.
  `bigiron/Slurm.md`), and `read_reference` also accepts the path inside the
  skill (`bigiron/References/Slurm.md`, or `../bigiron/References/Slurm.md`
  as written in a link from another reference). The system prompt names the
  site and tells Claude to load the site skill for site facts and never
  guess.

## Adding a site, step by step

1. Create `sites/<domain>/` with `Site.yaml`, `SKILL.md`, at least one
   `References/*.md` and a `QUESTIONS.md` for what you couldn't confirm,
   following the rules above. Add one folder per cluster (copy
   `sites/generic/_cluster-template/`) with its `Cluster.yaml` and
   references, and index the folders in `SKILL.md`.
2. Run `go test ./internal/site/ ./internal/skill/`: `TestEmbeddedSites` loads
   every profile and its skill next to the generic skills, and
   `TestGenericSkillsHaveNoSiteFacts` checks that none of the new site's terms
   are in the public skills.
3. Try it in Claude Code: copy the generic skills and the whole site folder,
   cluster folders included (as `site-<domain-with-dashes>`), into
   `~/.claude/skills/` (see `sites/README.md`) and ask a site question.
4. Keep the new profile out of this repository: `.gitignore` ignores every
   `sites/<domain>/` folder except `sites/generic/`.
