// Command labmill-winres writes the Windows resource object (.syso) of the
// desktop app: the Labmill icon (assets/icon/labmill.ico, 16-256 px) and the
// version information. `go build` links a *_windows_<arch>.syso in the
// package folder automatically, so the exe shows the Labmill icon in Explorer,
// on the taskbar and in its window title bar (Wails v2 loads the window icon
// from the icon group with resource ID 3).
//
// `make gui-windows` runs it before building (go run ./cmd/labmill-winres);
// the generated file is gitignored and overwritten on every build. Pure Go
// (github.com/tc-hib/winres, 0BSD), so it runs on Linux.
//
//	go run ./cmd/labmill-winres -arch amd64 -version 0.1.0 -o cmd/labmill-gui/rsrc_windows_amd64.syso
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tc-hib/winres"
	"github.com/tc-hib/winres/version"

	"labmill-client/assets/icon"
)

func main() {
	out := flag.String("o", "cmd/labmill-gui/rsrc_windows_amd64.syso", "output .syso file")
	arch := flag.String("arch", "amd64", "target architecture: amd64, arm64 or 386")
	ver := flag.String("version", "0.0.0", "file and product version (dotted numbers)")
	flag.Parse()
	if err := run(*out, *arch, *ver); err != nil {
		fmt.Fprintln(os.Stderr, "labmill-winres:", err)
		os.Exit(1)
	}
}

func run(out, arch, ver string) error {
	archs := map[string]winres.Arch{"amd64": winres.ArchAMD64, "arm64": winres.ArchARM64, "386": winres.ArchI386}
	a, ok := archs[arch]
	if !ok {
		return fmt.Errorf("unsupported arch %q", arch)
	}
	ico, err := winres.LoadICO(bytes.NewReader(icon.AppICO))
	if err != nil {
		return fmt.Errorf("load icon: %w", err)
	}
	rs := winres.ResourceSet{}
	// ID 3 (= RT_ICON's value, as the Wails CLI does): Wails v2's window
	// loads its icon from this group (winc.AppIconID), and as the first icon
	// group it is also the exe's icon in Explorer.
	if err := rs.SetIcon(winres.ID(3), ico); err != nil {
		return fmt.Errorf("set icon: %w", err)
	}
	var vi version.Info
	vi.SetFileVersion(ver)
	vi.SetProductVersion(ver)
	for k, v := range map[string]string{
		version.ProductName:      "labmill",
		version.FileDescription:  "labmill",
		version.InternalName:     "labmill-gui",
		version.OriginalFilename: "labmill-gui.exe",
		version.LegalCopyright:   "MIT License",
	} {
		if err := vi.Set(version.LangDefault, k, v); err != nil {
			return fmt.Errorf("version info: %w", err)
		}
	}
	rs.SetVersionInfo(vi)

	// Write next to the target, then rename, so a failed run never leaves a
	// half-written object for go build to link.
	tmp, err := os.CreateTemp(filepath.Dir(out), ".winres-*.tmp")
	if err != nil {
		return err
	}
	werr := rs.WriteObject(tmp, a)
	if werr == nil {
		werr = os.Chmod(tmp.Name(), 0o644)
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), out)
	}
	if werr != nil {
		os.Remove(tmp.Name()) // our own temp file
		return werr
	}
	return nil
}
