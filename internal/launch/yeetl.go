package launch

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"labmill-client/internal/config"
	"labmill-client/internal/sshconfig"
)

// ImportYeetl copies the setup of the yeetl client (the project Labmill was
// copied from) once: see importYeetl. Front ends call it before they read the
// configuration. It reports whether it copied a setup.
func ImportYeetl() (bool, error) {
	cfgPath, err := config.DefaultPath()
	if err != nil {
		return false, err
	}
	p, err := sshconfig.DefaultPaths()
	if err != nil {
		return false, err
	}
	return importYeetl(cfgPath, p, time.Now())
}

// importYeetl copies <config dir>/yeetl/clusters.yaml to cfgPath and
// <SSHDir>/config.d/yeetl to Labmill's SSH file (yeetl's "# yeetl-" markers
// become "# labmill-"), then adds Labmill's Include line. It does nothing when
// cfgPath already exists, when there is no yeetl clusters.yaml, or when
// Labmill can't parse it (for example a Run:ai cluster): the first-run setup
// then starts as usual. yeetl's own files are not changed, so yeetl keeps
// working.
func importYeetl(cfgPath string, p sshconfig.Paths, now time.Time) (bool, error) {
	if _, err := os.Stat(cfgPath); !errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(cfgPath)), "yeetl", "clusters.yaml"))
	if err != nil {
		return false, nil
	}
	if _, err := config.Parse(data); err != nil {
		return false, nil
	}
	if ssh, err := os.ReadFile(filepath.Join(p.SSHDir, "config.d", "yeetl")); err == nil {
		if _, err := os.Stat(p.LabmillConfig()); errors.Is(err, fs.ErrNotExist) {
			out := bytes.ReplaceAll(ssh, []byte("# yeetl-"), []byte("# labmill-"))
			if err := os.WriteFile(p.LabmillConfig(), out, 0o600); err != nil {
				return false, fmt.Errorf("copy yeetl's ssh settings: %w", err)
			}
		}
		if _, err := sshconfig.EnsureInclude(p, now); err != nil {
			return false, err
		}
	}
	if err := config.WriteFile(cfgPath, data); err != nil {
		return false, fmt.Errorf("copy yeetl's clusters.yaml: %w", err)
	}
	return true, nil
}
