# Contributing to Labmill client

Thank you for your help. Bug reports, ideas and pull requests are welcome.

## Issues

Open an issue on GitHub. Tell us your operating system, the Labmill version (`labmill --version`) and what you expected. Do not put passwords, API keys, tokens or private data from your cluster in an issue.

## Pull requests

- GitHub runs no tests on pull requests. The GitHub copy of this repository is a mirror; the tests run on our own server.
- A maintainer reads your pull request first. If it is a change we want, the maintainer copies it to a branch on our server, and our test sandbox runs the full test suite without network access. We merge the change on our server, and the mirror then shows it on GitHub. Your pull request is then closed with a link to the commit. Your authorship stays in the commit.
- This takes some days. Small pull requests that do one thing are tested faster.
- Before you send it, run the test gate on your computer:

  ```
  make check   # gofmt, go vet, go test, cross builds for windows/amd64 and darwin/arm64
  ```

  You need Go 1.26 or newer. The desktop app has its own targets (`make gui-linux`, `make gui-selftest`); see `docs/dev/testing.md`.

## Rules for changes

- **No information about a real institution** that is not public goes into this repository: no host names, user names, paths or internal documents. Use neutral examples such as `example.edu`, `bigcluster` and `/data/mygroup`. Only the generic site profile (`sites/generic/`) is in this repository.
- **Nothing leaves the user's computer without approval**, and no secret (password, two-factor code, API key) is written to disk. Changes that weaken this are not accepted.
- **Skills are shared with Claude Code.** A change to how Labmill does a task also goes into the skills in `skills/`, with the shell commands that a Claude Code user would run.
- The developer pages in `docs/dev/` describe the architecture, the security rules and the tests.

## License

By contributing, you agree that your contribution is licensed under the MIT license in [LICENSE](LICENSE).

## Security problems

Do not open a public issue for a security problem. Use GitHub's private vulnerability report ("Report a vulnerability" on the Security tab) instead.
