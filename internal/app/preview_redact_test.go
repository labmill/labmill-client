package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsDocOrCode(t *testing.T) {
	for _, n := range []string{"README.txt", "requirements.txt", "CMakeLists.txt", "README.md", "run.sh", "train.py", "Makefile", "Dockerfile", "LICENSE", "notes.md", "README.csv"} {
		if !isDocOrCode(n) {
			t.Errorf("%s should be doc or code", n)
		}
	}
	for _, n := range []string{"a.txt", "a.csv", "a.tsv", "a.fa", "a.fasta", "a.fastq", "a.pdb", "a.cif", "a.sam", "a.vcf", "a.bed", "a.gff", "a.json", "a.ipynb", "a.parquet", "a.npy", "a.h5", "a.log", "a.out", "notes.csv"} {
		if isDocOrCode(n) {
			t.Errorf("%s should be data", n)
		}
	}
}

func TestPreviewRedactsAndLimits(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(h.home, "data", "cfg")
	os.MkdirAll(dir, 0o755)
	conf := "name: demo\napi_key: sk-12345\nexport HF_TOKEN=hf_abc\n  \"password\": \"hunter2\",\nepochs: 3\n"
	os.WriteFile(filepath.Join(dir, "run.yaml"), []byte(conf), 0o644)
	out, err := h.run(t, "preview_file", map[string]any{"path": filepath.Join(dir, "run.yaml")})
	if err != nil || strings.Contains(out, "sk-12345") || strings.Contains(out, "hf_abc") || strings.Contains(out, "hunter2") ||
		!strings.Contains(out, "api_key:") || !strings.Contains(out, "epochs: 3") || !strings.Contains(out, "redacted") {
		t.Errorf("redaction: %q, %v", out, err)
	}
	// Names with password, and hard-secret files, are refused.
	for _, n := range []string{"passwords.md", "rclone.conf", "authentication.json"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x\n"), 0o644)
		if _, err := h.run(t, "preview_file", map[string]any{"path": filepath.Join(dir, n)}); err == nil {
			t.Errorf("%s previewed", n)
		}
	}
	// A doc file over 1 MiB falls back to the data sample with a note.
	big := filepath.Join(dir, "BIG.md")
	os.WriteFile(big, []byte(strings.Repeat("0123456789abcdef\n", 70000)), 0o644)
	out, err = h.run(t, "preview_file", map[string]any{"path": big, "lines": 100})
	if err != nil || strings.Count(out, "\n") > maxDataLines+2 || !strings.Contains(out, "over 1 MiB") {
		t.Errorf("big doc: %d lines, %v", strings.Count(out, "\n"), err)
	}
	if _, err := h.run(t, "preview_file", map[string]any{"path": big, "start_line": 50}); err == nil {
		t.Error("paging a >1 MiB file succeeded")
	}
}
