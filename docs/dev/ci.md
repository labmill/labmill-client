# GitHub Actions

The workflow files live in `.github/workflows/`.

## Workflows

| File | Runs on | What it does |
|---|---|---|
| `ci.yml` | pushes to `main`, every pull request | `make check` with the Go cache; `govulncheck`; a strict docs build; dependency review on pull requests. |
| `docs.yml` | push to `main`, pull requests, manual | Builds the docs strictly, as a check. The docs are published at `https://client.labmill.org/`, not from this workflow yet. |
| `build.yml` | push to `main`, tags `v*`, manual | Builds every binary and uploads each as a workflow artifact with a `.sha256` file (see below). On a `v*` tag it also creates a **draft** release with `gh release create`. |

`dependabot.yml` opens weekly updates for Go modules and for the actions themselves. Third-party actions are pinned to major versions.

## What `build.yml` produces

- `labmill` (TUI) for linux, darwin and windows, each amd64 and arm64, `CGO_ENABLED=0`.
- `labmill-gui` for Windows (cross-compiled on Linux with `make gui-windows`), Linux (`libgtk-3-dev` and `libwebkit2gtk-4.1-dev`, `make gui-linux`) and macOS arm64 (macos-latest, cgo, `-tags desktop,production`).

The GUI self-test also runs: `make gui-selftest` under `xvfb-run` on Linux, and the Windows self-test on windows-latest.

## Cutting a release

1. Make sure CI is green on `main`.
2. Tag and push: `git tag v0.1.0 && git push origin v0.1.0`.
3. `build.yml` builds everything and creates a draft release with all artifacts and a combined `SHA256SUMS`.
4. Review the draft and publish it by hand.

Signing is still open: Windows Authenticode and Apple Developer ID signing and notarizing need certificates, so these binaries are unsigned.
