# CLAUDE.md

Guidance for Claude Code (claude.ai/code) working in this repository.

## What it is

Labmill client: a single Go binary (`labmill`, TUI) plus a Wails desktop app (`labmill-gui`), a conversational front end to **Slurm** clusters. It is a fresh copy of the yeetl client and supports Slurm only. The user describes a compute task; Labmill finds the local data, stages it with a manifest (paths, sizes, checksums), transfers it, picks a cluster, writes and submits an `sbatch` script, monitors the job and pulls results back. It wraps the user's existing cluster account: no accounts or API of its own. Optional link to the user's Labmill account (app.labmill.org): `internal/account` sends finished-job records, never scripts, data, code, names or logs (`docs/dev/security.md`).

- `README.md` is user-facing function only. Architecture and rules live here and in `docs/dev/` (package table, request flow and tools: `docs/dev/architecture.md`). Docs are written with Zensical (`zensical.toml`, pages in `docs/`, published at `https://client.labmill.org/`).
- **Model access:** the agent loop runs in the client. By default it calls the Anthropic Messages API with the user's `ANTHROPIC_API_KEY` (read from the environment, never written to disk; planning turns `claude-opus-5-5`, others `claude-sonnet-5-5`; `LABMILL_MODEL` overrides both). An optional broker is used when `broker_url` (Site.yaml) or `LABMILL_BROKER_URL` is set, with the token from `LABMILL_BROKER_TOKEN`. Without a key and without a broker, a client linked with `labmill login` uses the Labmill test relay (owner decision 2026-10-26, testing only): the broker protocol against `<account URL>/api/model` with the program token (`chooseModel` in `internal/launch/model.go`). The own key always wins and is the private choice.
- **Files:** config `~/.config/labmill/clusters.yaml`, SSH settings `~/.ssh/config.d/labmill`, results `~/labmill-results`, job state in local SQLite. Go module path `labmill-client`.

## Code map

```
cmd/labmill (TUI) / cmd/labmill-gui (Wails) -> internal/launch.Build -> Session -> agent.Agent
    -> app tools (typed, Propose/approval) -> backend/slurm | transfer (rclone) | store (SQLite)
    -> sshx.Connect -> SSH helper -> cluster
agent -> Anthropic Messages API (internal/launch/model.go) or internal/broker -> optional broker
```

- `internal/launch` is the shared runtime; `cmd/*` stay thin; `internal/gui` is the GUI's pure-Go side.
- `internal/app` holds the typed tools, the prompt (`skillToolMap`), privacy rules (`privacy.go`) and the list of read-only remote commands (`remote.go`). Tools reach the outside only through `app.Env` function fields (tests use fakes).
- `skills/` and `sites/` are embedded Go packages. Untrusted text shown in the UIs goes through `textsafe.Clean`.
- TUI: Charm **v2**, `charm.land/...` import paths, never `github.com/charmbracelet/...` v1. Minimum **Go 1.26**.

## Rules that hold

- **No non-public or proprietary information about any institution goes into this repository (owner rule).** That covers code, tests, skills, site profiles, docs, commit messages and examples. Use neutral examples (`example.edu`, `bigcluster`, `/data/mygroup`).
- **Approval first.** Nothing leaves the machine until the user approves the file list and destination, unless the user turned on "Approve everything automatically" (off by default, banner, each step logged; secret-file reads, questions and logins still ask). Every `sbatch` script is inspectable and editable before submission.
- **No shell on the laptop.** Claude calls typed tools only, plus `remote_command {cluster, argv, script}` on the login node. Read-only commands run without asking; any other command shows its exact line for approval (consent, not a restriction) unless "Run commands on the cluster without asking" is on; reading a sign-in secret file always asks, and its output is redacted. Backends never run model-written shell: argv vectors are quoted with `backend.Quote`.
- **No restrictions on the remote HPC side.** Labmill constrains itself on the laptop, not on the cluster: no command lists that restrict, no refusal lists, no path limits there.
- **No secrets on disk.** SSH passwords, 2FA codes and the API key never touch disk. The one exception is the Labmill program token the user pastes (`labmill login`): `account.json` next to `clusters.yaml`, mode 0600, never logged or shown. Use the user's existing account, keys and login.
- **SSH is Go-native** (`x/crypto/ssh`), never the `ssh` binary; one shared connection via a helper that exits after about 10 idle minutes; settings in OpenSSH format in `~/.ssh/config.d/labmill`, with one `Include` at the top of `~/.ssh/config`; user entries are never rewritten. Key-only hosts get a guided key setup. Details: `docs/dev/ssh.md`.
- **Data transfer:** rclone in-process, file by file, never sync or delete; SFTP via the site's transfer host; S3 opt-in. Claude sees metadata and small samples, never bulk data.
- **Nothing installed or left running** except the desktop app in the tray (quits only from the tray menu) and the SSH helper. Nothing runs on clusters beyond Slurm.
- **Skills are the shared foundation.** Claude Code format; they must work alone in Claude Code and inside Labmill; skill shell steps map to typed tools (`skillToolMap`). Labmill is now the master copy of the generic Slurm skills (`skills/SOURCE.md`). Every change to domain logic also goes into the skills, with the shell commands a Claude Code user would run.
- **Where knowledge lives** (`docs/dev/knowledge-placement.md`): site facts, implicit ones included, live only in `sites/<domain>/`; only `sites/generic/` is in this repository (other profiles are gitignored and not part of the client). Generic knowledge lives in `skills/`. No site fact in skills, code, UI text or docs. Site schema: `docs/dev/sites.md`.
- **Naming under `sites/`:** institution and cluster folders are lowercase; every other folder and file starts with an uppercase letter (`References/`, `Site.yaml`, `Cluster.yaml`). `SKILL.md`, `README.md`, `QUESTIONS.md`, `SOURCE.md` and `embed.go` stay as they are.

## Build and test

Go 1.26 (`GOTOOLCHAIN=auto` in the Makefile). Details: `docs/dev/testing.md`.

```
make check          # the gate: gofmt, vet, tests, cross build+vet for windows/amd64 and darwin/arm64
make build          # bin/labmill (TUI)
make gui-linux      # bin/labmill-gui (cgo, webkit2gtk-4.1)
make gui-selftest   # GUI self-test under Xvfb (DOM assertions)
make gui-windows    # bin/cross/labmill-gui.exe
make docs           # zensical build --strict
go test ./internal/skill -run TestLoad   # one package / one test
```

- Live tests are opt-in (`LABMILL_LIVE=1`, plus `LABMILL_LIVE_SUBMIT=1`); they need an already-authenticated OpenSSH ControlMaster (`LABMILL_LIVE_ALIAS`) and skip otherwise.
- Temp files: `t.TempDir()`, or the gitignored `bin/ dist/ site/ tmp/`.

## Never use destructive commands

Don't use `rm`, `rmdir`, `shred`, `unlink`, `find -delete`, `xargs rm`, `sudo`, `dd`, `mkfs`, `git reset --hard`, `git checkout --` or `git push --force`. To drop a file, `mv` it into the gitignored `deleted/` folder (dated `yyyy-mm-dd-hh-mm` when it fits). Undo by fixing forward. Never prompt for or ask about such commands; if a task needs one, stop and report it.

## Subagents

`.claude/agents/`: `implementer` (fills designed function bodies), `reviewer` (read-only review before every commit), `docs-writer` (Zensical pages; `zensical build --strict` must pass). Commit only after `make check` passes and the reviewer approves.
