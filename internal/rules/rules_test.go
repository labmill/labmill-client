package rules

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The package may depend on no SSH, network or UI code: only the standard
// library and yaml.
func TestNoHeavyImports(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("go list: %v", err)
		}
		t.Skipf("go list unavailable: %v", err)
	}
	allowedOwn := map[string]bool{
		"labmill-client/internal/rules": true,
	}
	banned := []string{"golang.org/x/crypto/ssh", "net/http", "database/sql", "modernc.org/sqlite", "charm.land", "wails", "pkg/sftp", "rclone"}
	for _, p := range strings.Fields(string(out)) {
		if strings.HasPrefix(p, "labmill-client/") && !allowedOwn[p] {
			t.Errorf("rules depends on %s", p)
		}
		if p == "net" || p == "os/exec" {
			t.Errorf("rules depends on %s", p)
		}
		for _, b := range banned {
			if strings.Contains(p, b) {
				t.Errorf("rules depends on %s", p)
			}
		}
	}
}
