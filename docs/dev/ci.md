# GitHub Actions

The tests run on the Labmill server, not on GitHub. GitHub builds only the release files: the workflow `.github/workflows/release.yml` runs for a pushed `v*` tag and for nothing else. There is no workflow for pushes or pull requests (see `CONTRIBUTING.md`).

## What `release.yml` makes

| File | Built on | Smoke test on |
|---|---|---|
| `labmill-linux-amd64`, `labmill-linux-arm64`, `labmill-darwin-amd64`, `labmill-darwin-arm64`, `labmill-windows-amd64.exe`, `labmill-windows-arm64.exe` | one Linux job, `CGO_ENABLED=0` | each on its own platform's runner |
| `labmill-gui-linux-amd64`, `labmill-gui-linux-arm64` | `ubuntu-latest`, `ubuntu-24.04-arm` (cgo, webkit2gtk-4.1) | the same runner |
| `labmill-gui-darwin-arm64` | `macos-latest` (cgo) | the same runner |
| `labmill-gui-windows-amd64.exe` | `windows-latest` (icon and version resource from `labmill-winres`) | the same runner |
| `SHA256SUMS` | the release job | |

- The smoke test runs `--version` and expects `Labmill <tag>`. The full tests already ran on the server for that commit.
- The release job makes `SHA256SUMS`, a build provenance attestation, and a **draft** release. The owner checks the draft and publishes it.
- Security: the workflow has `contents: read`; only the release job has `contents: write` (and `id-token`, `attestations` for the attestation). Every action is pinned by its full commit SHA. The Go build cache is off. The workflow uses no secrets.
- The binaries are not signed. The macOS desktop app is not notarized; the release notes tell Mac users how to open it.

## Cutting a release

1. The commit passes the tests on the server.
2. The tag is made on the server and pushed to GitHub (after the owner's approval).
3. `release.yml` builds everything and makes the draft release.
4. The owner checks the draft and publishes it.

`dependabot.yml` opens weekly update pull requests for the Go modules and the pinned actions; they are tested on the server like every other pull request.
