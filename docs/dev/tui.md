# TUI stack: Charm

CLAUDE.md keeps the rule: Charm v2 libraries, `charm.land/...` import paths, never the v1 `github.com/charmbracelet/...` paths (Harmonica has no v2).

The TUI is built on Charm libraries. Every library except Harmonica is on **v2**, and the v2 releases use the **`charm.land/...` vanity import paths**, not `github.com/charmbracelet/...`. Don't mix in v1 imports. Many examples online still show the old paths and v1 APIs.

| Library | Import path | GitHub | Role in Labmill |
|---|---|---|---|
| Bubble Tea | `charm.land/bubbletea/v2` | https://github.com/charmbracelet/bubbletea | TUI framework (Elm-style model/update/view) for the chat pane and job dashboard |
| Bubbles | `charm.land/bubbles/v2` | https://github.com/charmbracelet/bubbles | Ready-made components: textarea for chat input, viewport, list, table, spinner, progress |
| Huh | `charm.land/huh/v2` | https://github.com/charmbracelet/huh | Forms and prompts: the confirm-before-moving-data step, choosing a cluster, the `clusters.yaml` setup wizard, and asking for the inputs a skill needs |
| Lip Gloss | `charm.land/lipgloss/v2` | https://github.com/charmbracelet/lipgloss | Styling and layout (panes, borders, colors) |
| Glamour | `charm.land/glamour/v2` | https://github.com/charmbracelet/glamour | Renders markdown in the terminal: Claude's replies, skill reference docs, and generated scripts shown for review |
| Log | `charm.land/log/v2` | https://github.com/charmbracelet/log | Logging, especially for headless Agent mode |
| Harmonica | `github.com/charmbracelet/harmonica` (v0.2, no v2) | https://github.com/charmbracelet/harmonica | Spring-physics animation for progress and transitions |
| Wish | `charm.land/wish/v2` | https://github.com/charmbracelet/wish | Serves Bubble Tea apps over SSH. Using it would add a server component, which conflicts with the "nothing left running permanently" rule in `CLAUDE.md` ("Design constraints to preserve"). Get it decided before relying on it. |

The current Charm v2 `go.mod` files declare `go 1.25`–`1.26`, which is why the project minimum is **Go 1.26**.

