# Where knowledge lives

Labmill's know-how comes from two places: the **generic skills** and the **site profiles**. This page states the rules for which knowledge goes where.

## The rules

1. **Site-specific information lives only in `sites/<domain>/`.** Only the public `sites/generic/` fallback is in this repository. Profiles for real institutions are not part of the Labmill client; they are kept and shipped apart from it. Site-specific means anything that is true of one institution or one of its clusters, and not of research computing in general:
    - **Slurm configuration:** partitions, queues, QOS, accounts, limits, and how they are configured.
    - **Software:** where it can be found, what is already installed (modules, versions, images, drivers, CUDA), and container registries.
    - **Access:** how to log in, bastion and login hosts, transfer hosts, 2FA, proxies, and how to get an account.
    - **Storage:** data storage paths and their specific configuration (quotas, purge rules, scratch areas).
    - **Hardware and support:** GPU and node inventories, documentation and help-desk URLs.
    - **Implicit facts:** a number, model name or anecdote that only one site has, even without its name. Examples are a node's core count, the site's CUDA floor, or "it converged on GPU X but not Y" where X and Y are one site's GPUs.
2. **All generic knowledge lives in the skills (`skills/`).**
    - Slurm manuals and best practices.
    - General research computing: virtual environments, conda, Jupyter notebooks, containers, POSIX file systems, storage tiers, SSH, shell hygiene, data transfer and credentials.
    - Research computing support: how to get help and how to write a good ticket.

    A site profile holds only the site's values, and points to the generic skill for everything else.
3. **No site-specific information may ever appear in the skills.** That covers `skills/` in full, its provenance notes included. It also applies to Labmill's runtime code and embedded data (system prompt, tool descriptions, catalogs, defaults, UI text), to the public `sites/README.md`, and to all documentation. These read site values from `Site.yaml` / `Cluster.yaml` at run time, or use neutral examples (`example.edu`, `bigcluster`, `/data/mygroup`).
4. **The README and the docs stay anonymous.** `README.md`, `CLAUDE.md` and everything under `docs/` name no institution, host, cluster, person or path of a real site.
5. **Skills hold all the logic.** A Claude Code user who installs the generic skills plus their site skill gets everything Labmill knows. Every change to domain logic also goes into the skills, with the shell commands a Claude Code user would run.

## How it stays enforced

- The leak test (`internal/site/leak_test.go`, `TestGenericSkillsHaveNoSiteFacts`) checks the embedded generic skills against every term derived from the site profiles that are present, plus curated `forbidden_terms` for implicit facts.
- The drift test (`internal/app/skillmap.go`) ties each typed tool's logic to a skill section.
- Every feature's acceptance includes "skills updated". The reviewer checks both directions:
    - no site facts in skills, code or docs;
    - no generic teaching in a site profile.
