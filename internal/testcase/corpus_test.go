package testcase

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const casesDir = "../../tests/cases"

func loadCorpus(t *testing.T) []Case {
	t.Helper()
	cases, errs := Load(os.DirFS(casesDir), ".")
	for _, e := range errs {
		t.Error(e)
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	return cases
}

func needSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("cases use sh")
	}
}

// treeHash hashes every file name and content under dir.
func treeHash(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	var names []string
	if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, p)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(dir, n)
		fmt.Fprintf(h, "%s\x00%d\x00%s", rel, len(b), b)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func runGen(t *testing.T, c Case) string {
	t.Helper()
	work := t.TempDir()
	out, err := exec.Command("sh", filepath.Join(casesDir, c.Dir, "gen.sh"), work).CombinedOutput()
	if err != nil {
		t.Fatalf("%s gen.sh: %v\n%s", c.ID, err, out)
	}
	var size int64
	filepath.WalkDir(work, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, e := d.Info(); e == nil {
				size += fi.Size()
			}
		}
		return nil
	})
	if size > int64(c.DataBytesMax) {
		t.Errorf("%s: data %d bytes, over data_bytes_max %d", c.ID, size, c.DataBytesMax)
	}
	return work
}

func TestCorpusValid(t *testing.T) { loadCorpus(t) }

func TestGenDeterministic(t *testing.T) {
	needSh(t)
	for _, c := range loadCorpus(t) {
		if a, b := treeHash(t, runGen(t, c)), treeHash(t, runGen(t, c)); a != b {
			t.Errorf("%s: gen.sh gives different data on a second run", c.ID)
		}
	}
}

var (
	pngOK  = "\x89PNG\r\n\x1a\n" + strings.Repeat("x", 1200)
	pngBad = "not a png " + strings.Repeat("x", 1200)
)

// checkCases holds, for each case with a check.sh, the files of a right
// result and of a wrong one. The check must accept the first and refuse the second.
var checkCases = map[string]struct{ right, wrong map[string]string }{
	"py-numpy-sum":       {map[string]string{"result.txt": "499995.00\n"}, map[string]string{"result.txt": "1\n"}},
	"py-stdin-stdout":    {map[string]string{"count.txt": "500\n"}, map[string]string{"count.txt": "499\n"}},
	"py-pandas-groupby":  {map[string]string{"totals.csv": "region,total\nr0,60300\nr1,59900\nr2,60100\n"}, map[string]string{"totals.csv": "region,total\nr0,60300\nr1,59900\nr2,1\n"}},
	"py-matplotlib-png":  {map[string]string{"plot.png": pngOK}, map[string]string{"plot.png": pngBad}},
	"py-multiprocessing": {map[string]string{"count.txt": "17984\n"}, map[string]string{"count.txt": "17983\n"}},
	"py-venv-pip-small":  {map[string]string{"result.txt": "six\n"}, map[string]string{"result.txt": "seven\n"}},
	"r-lm-summary":       {map[string]string{"slope.txt": "2.00\n"}, map[string]string{"slope.txt": "3.00\n"}},
	"r-ggplot-png":       {map[string]string{"plot.png": pngOK}, map[string]string{"plot.png": pngBad}},
	"r-parallel-apply":   {map[string]string{"total.txt": "2668667000\n"}, map[string]string{"total.txt": "2.668667e+09\n"}},
	"array-10-tasks": {
		map[string]string{"out-1.txt": "1", "out-2.txt": "4", "out-3.txt": "9", "out-4.txt": "16", "out-5.txt": "25", "out-6.txt": "36", "out-7.txt": "49", "out-8.txt": "64", "out-9.txt": "81", "out-10.txt": "100"},
		map[string]string{"out-1.txt": "1"},
	},
}

func TestCheckScripts(t *testing.T) {
	needSh(t)
	for _, c := range loadCorpus(t) {
		if c.Expect.Outcome == "failed" {
			continue
		}
		tc, ok := checkCases[c.ID]
		if !ok {
			t.Errorf("%s: add a right and a wrong result to checkCases", c.ID)
			continue
		}
		work := runGen(t, c)
		run := func(files map[string]string) error {
			res := t.TempDir()
			for n, body := range files {
				os.WriteFile(filepath.Join(res, n), []byte(body), 0o644)
			}
			return exec.Command("sh", filepath.Join(casesDir, c.Dir, "check.sh"), res, work).Run()
		}
		if err := run(tc.right); err != nil {
			t.Errorf("%s: right result refused: %v", c.ID, err)
		}
		if err := run(tc.wrong); err == nil {
			t.Errorf("%s: wrong result accepted", c.ID)
		}
	}
}

// forbidden matches what must not be in a public case: a real institution
// host (anything but example.*), a secret word, a key block.
var forbidden = regexp.MustCompile(`(?i)[a-z0-9-]+\.(edu|gov|ac\.[a-z]{2})\b|password|passwd|api[_-]?key|secret|token|BEGIN [A-Z ]*PRIVATE KEY`)

func TestNoForbiddenWords(t *testing.T) {
	err := filepath.WalkDir(casesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range forbidden.FindAllString(string(b), -1) {
			if !strings.HasPrefix(strings.ToLower(m), "example.") {
				t.Errorf("%s: forbidden word %q", p, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
