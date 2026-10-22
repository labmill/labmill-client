package transfer

import (
	"path/filepath"
	"strings"
	"testing"

	"labmill-client/internal/backend"
)

// A sample folder under "OneDrive - Example University" with spaces in the
// folder and file names is staged with slash-separated relative paths and
// nothing skipped (M38.4).
func TestBuildManifestOneDriveSpaces(t *testing.T) {
	root := filepath.Join(t.TempDir(), "OneDrive - Example University", "Desktop", "My Sample")
	mwrite(t, filepath.Join(root, "README.md"), "r")
	mwrite(t, filepath.Join(root, "test data", "small file.in"), "1 2 3")
	got, skipped := manifestPaths(t, root, []string{"."})
	if strings.Join(got, "|") != "README.md|test data/small file.in" || len(skipped) != 0 {
		t.Errorf("got %v skipped %v", got, skipped)
	}
	got, _ = manifestPaths(t, root, []string{"test data"})
	if strings.Join(got, "|") != "test data/small file.in" {
		t.Errorf("folder with a space: %v", got)
	}
}

// Local and remote paths with spaces are quoted as one shell word in the
// shown rclone commands (POSIX and PowerShell) and in the remote checksum
// command.
func TestRemotePathsWithSpacesQuoted(t *testing.T) {
	r := Remote{Dir: "/storage/lab/labmill/my-sample"}
	local := "OneDrive - Example University/My Sample"
	cmd := r.rcloneCommand("linux", local, []string{"a b.txt"}, true)
	if !strings.Contains(cmd, "'"+local+"'") {
		t.Errorf("posix upload: %s", cmd)
	}
	cmd = r.rcloneCommand("windows", `C:\Users\Ann Lee\OneDrive - Example University\My Sample`, []string{"a b.txt"}, true)
	if !strings.Contains(cmd, `'C:\Users\Ann Lee\OneDrive - Example University\My Sample'`) {
		t.Errorf("windows upload: %s", cmd)
	}
	cmd = r.rcloneCommand("linux", local, []string{"test data/small file.in"}, false)
	if !strings.Contains(cmd, "test data/small file.in'") {
		t.Errorf("download: %s", cmd)
	}
	q := backend.Quote(sumArgv(r, []string{"test data/small file.in"}))
	if q != "sha256sum -- '/storage/lab/labmill/my-sample/test data/small file.in'" {
		t.Errorf("sum command = %s", q)
	}
}
