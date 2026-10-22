# Testing

Every package is tested without network access, real clusters or real credentials. Interfaces at the package boundaries (`agent.Model`, `agent.Approver`, the model client, the SSH directory, the store path) let tests substitute in-process fakes.

## Running tests

```
make check                        # gofmt, go vet, go test ./..., cross-compile for Windows and macOS
go test ./...                     # all tests
go test ./internal/skill          # one package
go test ./internal/skill -run TestLoad     # tests whose name matches the regex
go test -race ./...               # with the race detector
go test -count=1 ./internal/store # bypass the test cache
```

`make check` is the gate for every commit. Run `go test -race` for any change that touches goroutines or the store, since the store is meant to be used by several processes at once.

## How tests are organized

Tests sit next to the code in `*_test.go` files. Files named `review_test.go` hold regression tests added after code review.

- **No network.** The broker tests start an `httptest` server on loopback as a fake broker and point `broker.Config.BaseURL` at it (plain `http` is allowed only for loopback). They also set `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` and `ANTHROPIC_BASE_URL` with `t.Setenv` to check that the client ignores them.
- **Scripted model.** The agent tests use a `scriptedModel` that implements `agent.Model` and returns prepared replies in order, with helpers to build text and tool-use messages. A `fakeApprover` records proposals and returns canned decisions. Together they cover declines, edited proposals, aborts, truncation and `MaxTurns` without calling any model.
- **Temp directories instead of the real home.** `config` and `store` tests write under `t.TempDir()` and set `XDG_CONFIG_HOME` or `XDG_STATE_HOME` with `t.Setenv`. The `sshconfig` functions take a `sshconfig.Paths{SSHDir: ...}` value, so tests point `SSHDir` at a temporary directory and never touch `~/.ssh`. Skill override tests build override directories (including escaping symlinks) in a temp directory.
- **Two-handle store tests.** Because several Labmill processes share one database, the store tests open the same file twice with `store.Open` and use both handles, for example to tail events from one handle while the other writes, and to run concurrent `UpdateJob` calls from two handles and check that no update is lost.
- **Embedded skills.** The skill tests load from an in-memory `fs.FS` for the loader rules and from `skills.FS` to check that the vendored skills are valid.

## SSH test server: `sshtest`

`internal/sshx/sshtest` is an in-process SSH server for tests. It mimics a typical campus login flow: keyboard-interactive password, then a Duo-style menu where `1` sends a push. The `Duo` modes are: no 2FA, push approved after a delay (or a passcode), push never approved, auto-push without a menu (approved or never), not enrolled, and push denied. It also provides:

- scripted exec commands (`Handler` functions standing in for `sinfo`, `sbatch`, `squeue`, ...), which receive the arguments verbatim and return an exit status;
- the sftp subsystem (`github.com/pkg/sftp`), backed by a real directory;
- direct-tcpip forwarding, for ProxyJump tests;
- a known-hosts line, so tests verify host keys without touching the developer's `~/.ssh`.

Tests point `sshconfig.Paths` at a temporary `SSHDir`, so they stay offline and never use real keys. The helper, the Slurm runner, `ssh-exec` and the transfers are all tested against this server.

## The end-to-end harness (`internal/e2e`)

`internal/e2e` holds test code only. A scripted model drives the real agent loop through the real `app` tools, the Slurm backend over the SSH helper, rclone uploads and downloads through `ssh-exec`, and the SQLite store, against the in-process SSH server acting as the cluster. The Slurm test walks the full flow: find, stage, upload (approval), draft, submit (approval), status, fetch (approval). It checks the uploaded bytes on the fake cluster, that the approved script is what reached `sbatch`, the fetched results and the final job state.

Because rclone runs `labmill ssh-exec` as a child process, the e2e `TestMain` re-executes the test binary as `ssh-exec` when `LABMILL_E2E_SSHEXEC=1` is set. The tests skip under `-short`, since they spawn processes.

## Why some tests are excluded from `-race`

The e2e tests carry `//go:build !race`. rclone v1.75.1's external-ssh connection pooling has an internal data race (`sshSessionExternal.exited` in `backend/sftp/ssh_external.go` reads `cmd.ProcessState` while `exec.Cmd.Wait` writes it), which fires during real transfers. It is not in Labmill's code, so the tests are skipped under the race detector instead of failing it. Run them with plain `go test ./internal/e2e/`. The transfer tests work around the same race. Everything else is race-tested: `go test -race ./...` still covers the store, the helper, the agent and the TUI.

## Live smoke test

`internal/backend/slurm/live_test.go` (`TestLiveReadOnly`) parses real `sinfo` and `squeue` output from a real Slurm cluster. It is skipped unless `LABMILL_LIVE=1`. It needs an already-authenticated OpenSSH ControlMaster that you opened yourself (set `LABMILL_LIVE_ALIAS` to its SSH alias; there is no default and the test skips when it is unset; check with `ssh -O check <alias>`), and it only runs read-only commands.

```
LABMILL_LIVE=1 LABMILL_LIVE_ALIAS=<alias> go test -run TestLiveReadOnly ./internal/backend/slurm/
```

The Slurm live tests that submit (`LABMILL_LIVE_SUBMIT=1`) send only short CPU jobs named `labmill-live-*` and cancel only their own jobs. `internal/launch/live_guard_test.go` holds the guard approver used by the live runs; it approves only steps that stay inside the test's own folder.

## Live S3 check

`TestLiveS3` (`internal/transfer/live_s3_test.go`) runs Labmill's real `UploadS3`/`DownloadS3` against real S3. It never creates or deletes buckets or objects (re-runs overwrite). Create the buckets yourself first (private, public access blocked; one with default SSE-KMS using the `aws/s3` key), then:

```
LABMILL_LIVE=1 LABMILL_LIVE_S3=1 LABMILL_LIVE_S3_BUCKET=<plain bucket> \
  LABMILL_LIVE_S3_KMS_BUCKET=<kms bucket> [LABMILL_LIVE_S3_REGION=us-west-2] [LABMILL_LIVE_S3_PROFILE=<profile>] \
  go test -run TestLiveS3 -v ./internal/transfer/
```

Subtests: `plain` (small files, an empty file, a 210 MiB random file uploaded multipart in 42 parts, download, byte compare, re-upload skips); `kms-default-encryption` (the same on a bucket whose default encryption is SSE-KMS); `kms-explicit-sse` (`SSE: "aws:kms"`); `wrong-region` (region set to us-east-1 for a us-west-2 bucket).

Findings from the first runs:

- **Bug found and fixed:** on a bucket with default SSE-KMS the ETag is a 32-hex non-MD5, and rclone (not told about the encryption) failed the upload with `md5 hashes differ`; multipart would fail the same way. `UploadS3`/`DownloadS3` now detect that error and repeat once with rclone's checksum comparison and multipart-ETag check off (S3's Content-MD5 check on every request and part, and Labmill's size check, stay). `DownloadS3` repeats only after a HeadObject shows the failing object is KMS (or SSE-C) encrypted, and then sets `server_side_encryption` on the source so rclone checks the md5chksum metadata where present; it never ignores checksums, and a mismatch on any other object is an error. Covered offline by `TestS3NonMD5ETag`, `TestS3NonMD5ETagCorruptBodyFails` (a body damaged after Content-MD5 was set is still rejected) and `TestS3DownloadMismatchWithoutKMSFails` (fake S3 with bogus ETags).
- rclone may remove its own failed upload under the prefix; the test never deletes anything itself.
- Wrong region: rclone follows S3's redirect, no error, so there is nothing to explain.
- A named non-default profile (`LABMILL_LIVE_S3_PROFILE`) works. SSO profiles: not tested (no SSO profile in `~/.aws/config`).

## Fuzzing

`FuzzQuoteSplit` in `internal/backend/review_m7_test.go` checks that `backend.Quote` and `backend.SplitWords` round-trip for arbitrary strings. As an ordinary test it runs the seed corpus on every `go test`. To fuzz for real:

```
go test ./internal/backend -run '^$' -fuzz FuzzQuoteSplit -fuzztime 30s
```

## Other conventions

- **TUI tests** use Bubble Tea's `teatest` helpers and an injectable clock (`model.now`), so the half-second key grace after a modal opens can be tested without sleeping.
- **Cross-platform code.** Privacy rules, the helper's lock and spawn code sit behind build tags. `make check` only cross-compiles them for Windows and macOS; some checks need real hardware.

## Build targets and test setup

Minimum **Go 1.26** (`go.mod`: `go 1.26.0`, `toolchain go1.26.8`), which Charm v2 needs. The Makefile exports `GOTOOLCHAIN=auto`.

```
make check                     # the gate: gofmt check, go vet, go test ./..., cross build+vet for windows/amd64 and darwin/arm64
make build                     # bin/labmill (TUI)
go test ./internal/skill -run TestLoad      # one package / one test
go test -race ./...            # needed when touching goroutines, the store or the helper
go test ./internal/e2e/        # e2e tests are //go:build !race (rclone's own data race) and skip under -short
make docs                      # zensical build --strict (zensical.toml, docs/ -> site/; uv tool install zensical)
make gui-linux                 # bin/labmill-gui, tags desktop,production,webkit2_41 (cgo + webkit2gtk-4.1); not in make check
make gui-smoke / gui-selftest  # run the GUI under a throwaway HOME in tmp/, on a hidden Xvfb display (LABMILL_GUI_VISIBLE=1 shows it); selftest asserts on the real DOM
make gui-windows / gui-windows-selftest   # cross-built exe, self-test on the Windows host via WSL interop (WebView2)
```

- **Live tests are opt-in** (skip otherwise): `LABMILL_LIVE=1` (read-only, needs a live ControlMaster for `LABMILL_LIVE_ALIAS`), `LABMILL_LIVE_SUBMIT=1` for job-submitting tests.
- **Offline test doubles:** `internal/sshx/sshtest` (in-process SSH server that mimics a password + Duo-menu login, with scripted exec commands, sftp and direct-tcpip), `agent/agenttest` (fake broker), `review_test.go` regressions; tests needing rclone's `ssh-exec` child re-exec the test binary (`LABMILL_E2E_SSHEXEC` / `LABMILL_TEST_SSHEXEC`).
- **GitHub Actions** (`.github/workflows/`): see [CI](ci.md).


## GUI self-tests

`make gui-selftest` runs the Wails GUI on a hidden Xvfb display. `make gui-windows-selftest` runs the cross-built exe in WebView2 on the Windows host through WSL interop; its window is never shown (no focus change, no taskbar button; the tray icon shows briefly) and the test proves that with `IsWindowVisible`/`GetForegroundWindow` on the app's own windows. The self-test also covers the jobs panel divider (section 6b: keys, drag, double-click reset, the saved width through `SettingsAPI`); the prefs field has Go tests in `internal/gui/prefs_test.go` (round trip, clamping). Details in `gui.md`.

Startup timing (`internal/startup`): every startup step records a mark or span; the lines (`startup: <step> <ms> ms`) go to `labmill.log` when the chat is usable (again after 20 s for the background steps) and are printed by the self-test. `LABMILL_GUI_TIMING=1 make gui-selftest` (or `gui-windows-selftest`) also runs the real `launch.Build` on a persistent home per site next to the executable, `labmill-selftest-timing-home-<domain>`, reused across runs so the second run is a warm start (`LABMILL_GUI_TIMING_SITE=<domain>` picks the site profile, default `generic`), so the numbers cover the whole startup.
