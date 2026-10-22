# Developer documentation

- [Architecture](architecture.md): packages and how they fit together.
- [SSH](ssh.md): the Go-native SSH client, sign-in and the shared connection.
- [TUI](tui.md): the terminal interface and its Charm libraries.
- [Desktop app (GUI)](gui.md): the Wails app, its build and self-test.
- [Voice](voice.md): the planned "Hey Labmill" design.
- [Testing](testing.md): how tests are organized and run.
- [Security](security.md): the protections that exist in the code.
- [Site profiles](sites.md): the `Site.yaml` and `Cluster.yaml` schema.
- [Where knowledge lives](knowledge-placement.md): skills against site profiles.
- [CI](ci.md): the GitHub Actions workflows.

The design rules are in `CLAUDE.md` at the repository root.

## Building and checking

```
make check   # gofmt, go vet, go test, cross-compile for Windows and macOS
make build   # bin/labmill
make docs    # zensical build --strict
```

Go 1.26 or newer is required. With `GOTOOLCHAIN=auto` (the default), an older `go` command downloads the right toolchain automatically.
