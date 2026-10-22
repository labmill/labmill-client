# The single gate for every commit in the development loop (see CLAUDE.md).
GO ?= go
export GOTOOLCHAIN ?= auto

.PHONY: check fmt conflicts vet test cross build docs

check: fmt conflicts vet test cross

fmt:
	@out="$$(gofmt -l $$(git ls-files '*.go' 2>/dev/null; git ls-files --others --exclude-standard '*.go'))"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

# Merge-conflict markers left in any file (tracked or new) fail the gate;
# Markdown would otherwise pass.
conflicts:
	@out="$$( { git ls-files; git ls-files --others --exclude-standard; } | sort -u | xargs -d '\n' grep -lE '^(<<<<<<<|>>>>>>>) ' 2>/dev/null)"; \
	if [ -n "$$out" ]; then echo "merge-conflict markers in:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

# Platform code must keep compiling for the GUI/voice targets we can't run here.
# Output goes to bin/cross/ (gitignored, overwritten) so no stray binaries appear.
# vet per OS also compiles the _test.go files, which build alone skips.
cross:
	GOOS=windows GOARCH=amd64 $(GO) build -o bin/cross/windows/ ./...
	GOOS=darwin GOARCH=arm64 $(GO) build -o bin/cross/darwin/ ./...
	GOOS=windows GOARCH=amd64 $(GO) vet ./...
	GOOS=darwin GOARCH=arm64 $(GO) vet ./...

build:
	$(GO) build -o bin/labmill ./cmd/labmill

docs:
	zensical build --strict

# Desktop app (Wails v2, system webview; needs webkit2gtk-4.1 and cgo). Kept
# out of `make check`: the package is behind the `desktop` build tag.
# macOS and Windows builds use the same command on those hosts (drop webkit2_41).
GUI_TAGS ?= desktop,production,webkit2_41

.PHONY: gui-linux gui-smoke gui-selftest gui-windows gui-windows-selftest
gui-linux:
	$(GO) build -tags $(GUI_TAGS) -o bin/labmill-gui ./cmd/labmill-gui

# Both targets run the app with a throwaway HOME under tmp/ (gitignored), so
# the real configuration is never touched. The folder is left behind (tmp/ is
# scratch) because the Makefile never deletes anything.

# The Linux GUI targets run on a hidden X server (Xvfb) when xvfb-run is
# installed, so no window opens on the desktop (WSLg would show one on the
# Windows screen). LABMILL_GUI_VISIBLE=1 shows the window instead.
GUI_RUN = $(if $(and $(shell command -v xvfb-run 2>/dev/null),$(if $(LABMILL_GUI_VISIBLE),,1)),env -u WAYLAND_DISPLAY xvfb-run -a -s "-screen 0 1400x900x24",)

# Launches the app on the first-run setup page; it quits itself 3 s after the
# page loads and exits non-zero unless the page called Setup.State (that is,
# unless app.js ran).
gui-smoke: gui-linux
	@d="$$(pwd)/tmp/gui-smoke-$$$$"; mkdir -p "$$d"; \
	HOME="$$d" XDG_CONFIG_HOME="$$d/.config" XDG_DATA_HOME="$$d/.local/share" XDG_STATE_HOME="$$d/.local/state" XDG_CACHE_HOME="$$d/.cache" \
	LABMILL_GUI_SMOKE=1 $(GUI_RUN) timeout 30 bin/labmill-gui; rc=$$?; \
	echo "labmill-gui exit status: $$rc (temp HOME $$d)"; exit $$rc

# Startup timing (M46.1): LABMILL_GUI_TIMING=1 [LABMILL_GUI_TIMING_SITE=<domain>] also runs launch.Build
# on a persistent per-site timing home and prints one "startup: <step> <ms> ms" line per step (also the Windows target).
# In-webview self-test: a scripted fake session drives the real dialogs and
# frontend/selftest.js checks the real DOM (exit 0 passed, 1 failed, 2 timeout).
# Once the app has hidden its window to the tray it writes "ready" to
# bin/labmill-selftest-second (overwritten each run); then a second copy is
# launched: it must hand over to the running one (single-instance lock) and
# exit at once (status 0 or 1; 124 means it started a second app).
gui-selftest: gui-linux
	$(GO) test -tags $(GUI_TAGS) ./cmd/labmill-gui -run 'TestGate|TestTiming|TestSplash'
	@d="$$(pwd)/tmp/gui-selftest-$$$$"; mkdir -p "$$d"; m=bin/labmill-selftest-second; echo wait > $$m; \
	export HOME="$$d" XDG_CONFIG_HOME="$$d/.config" XDG_DATA_HOME="$$d/.local/share" XDG_STATE_HOME="$$d/.local/state" XDG_CACHE_HOME="$$d/.cache"; \
	out="$$($(GUI_RUN) sh -c 'LABMILL_GUI_SELFTEST=1 LABMILL_GUI_SELFTEST_SECOND=1 timeout 60 bin/labmill-gui & pid=$$!; \
	rc2=skipped; for i in $$(seq 120); do kill -0 $$pid 2>/dev/null || break; \
	  if [ "$$(cat '"$$m"')" = ready ]; then LABMILL_GUI_SELFTEST=1 timeout 15 bin/labmill-gui >/dev/null 2>&1; rc2=$$?; break; fi; sleep 0.5; done; \
	wait $$pid; echo "@@rc=$$? rc2=$$rc2"' | tee /dev/stderr | tail -1)"; \
	rc="$${out#@@rc=}"; rc="$${rc%% *}"; rc2="$${out##* rc2=}"; \
	echo "second launch exit status: $$rc2"; [ "$$rc2" != 124 ] || rc=1; \
	echo "labmill-gui self-test exit status: $$rc (temp HOME $$d)"; exit $$rc

# Windows GUI: Wails on Windows needs no cgo, so it cross-compiles from here.
# labmill-winres first writes the exe's icon and version resource (gitignored
# .syso, overwritten each build; go build links it automatically).
gui-windows:
	$(GO) run ./cmd/labmill-winres -arch amd64 -o cmd/labmill-gui/rsrc_windows_amd64.syso
	GOOS=windows GOARCH=amd64 $(GO) build -tags desktop,production -trimpath -ldflags "-H windowsgui -s -w" -o bin/cross/labmill-gui.exe ./cmd/labmill-gui

# Run the self-test on the Windows host through WSL interop (WebView2). The
# exe is copied to %TEMP%\labmill-selftest (overwritten each run); self-test mode
# builds no runtime and writes no Labmill files except labmill-selftest-gui.json,
# the labmill-selftest-folders tree (folder-button checks, and the Settings
# page's fixture clusters.yaml and SSH files with their backups; created,
# never deleted) and the marker file labmill-selftest-second, all next to the exe. Its tray icon
# shows briefly, but the window stays hidden and never takes focus (StartHidden;
# the self-test checks it with EnumWindows/IsWindowVisible/GetForegroundWindow). A second copy is launched as in gui-selftest.
gui-windows-selftest: gui-windows
	@command -v cmd.exe >/dev/null || { echo "needs WSL with Windows interop"; exit 2; }; \
	wt="$$(cmd.exe /c 'echo %TEMP%' 2>/dev/null | tr -d '\r')"; d="$$(wslpath "$$wt")/labmill-selftest"; \
	mkdir -p "$$d" && cp bin/cross/labmill-gui.exe "$$d/" && cd "$$d" && echo wait > labmill-selftest-second && echo none > labmill-selftest-notray && \
	{ LABMILL_GUI_SELFTEST=1 LABMILL_GUI_SELFTEST_SECOND=1 WSLENV=LABMILL_GUI_SELFTEST:LABMILL_GUI_SELFTEST_SECOND:LABMILL_GUI_TIMING:LABMILL_GUI_TIMING_SITE timeout 90 ./labmill-gui.exe & pid=$$!; \
	rc2=skipped; for i in $$(seq 160); do kill -0 $$pid 2>/dev/null || break; \
	  if [ "$$(cat labmill-selftest-second)" = ready ]; then LABMILL_GUI_SELFTEST=1 WSLENV=LABMILL_GUI_SELFTEST timeout 15 ./labmill-gui.exe >/dev/null 2>&1; rc2=$$?; break; fi; sleep 0.5; done; \
	wait $$pid; rc=$$?; }; \
	echo "second launch exit status: $$rc2"; [ "$$rc2" != 124 ] || rc=1; \
	nt="$$(cat labmill-selftest-notray)"; echo "second launch skipped the early tray: $$nt"; \
	[ "$$rc2" = skipped ] || [ "$$nt" = skipped ] || { echo "FAIL: the second process started a tray"; rc=1; }; \
	echo "labmill-gui.exe self-test exit status: $$rc ($$wt\\labmill-selftest)"; exit $$rc
