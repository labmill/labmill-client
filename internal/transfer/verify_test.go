package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"labmill-client/internal/sshx/sshtest"
)

// shellSplit undoes backend.Quote's POSIX quoting (single and double quotes,
// backslash), like the remote login shell would.
func shellSplit(s string) []string {
	var out []string
	var cur strings.Builder
	in := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			in = true
			j := strings.IndexByte(s[i+1:], '\'')
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case c == '"':
			in = true
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				cur.WriteByte(s[i])
			}
		case c == '\\' && i+1 < len(s):
			in = true
			i++
			cur.WriteByte(s[i])
		case c == ' ':
			if in || cur.Len() > 0 {
				out = append(out, cur.String())
			}
			cur.Reset()
			in = false
		default:
			in = true
			cur.WriteByte(c)
		}
	}
	if in || cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// fakeSha256sum is a scripted sha256sum over root. lie names paths (as
// given) to report a wrong sum for; calls records each argument list.
type fakeSha256sum struct {
	mu   sync.Mutex
	root string
	lie  map[string]bool
	hide map[string]bool // paths reported unreadable
	// blockAfter > 0 makes every call after that many calls block (the
	// remote hangs), long past any test timeout.
	blockAfter int
	calls      [][]string
	release    chan struct{} // closed at test end to free hung calls
}

func (f *fakeSha256sum) handler(args string, _ io.Reader, stdout, stderr io.Writer) int {
	argv := shellSplit(args)
	f.mu.Lock()
	f.calls = append(f.calls, argv)
	block := f.blockAfter > 0 && len(f.calls) > f.blockAfter
	f.mu.Unlock()
	if block {
		select {
		case <-f.release:
		case <-time.After(10 * time.Second):
		}
		return 0
	}
	if len(argv) == 0 || argv[0] != "--" {
		fmt.Fprintln(stderr, "sha256sum: missing --")
		return 2
	}
	status := 0
	for _, p := range argv[1:] {
		b, err := os.ReadFile(filepath.Join(f.root, p))
		if err != nil || f.hide[p] {
			fmt.Fprintf(stderr, "sha256sum: %s: Permission denied\n", p)
			status = 1
			continue
		}
		sum := sha256.Sum256(b)
		h := hex.EncodeToString(sum[:])
		if f.lie[p] {
			h = strings.Repeat("0", 64)
		}
		name, prefix := p, ""
		if strings.ContainsAny(p, "\\\n") { // GNU escaping
			name = strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(p)
			prefix = `\`
		}
		fmt.Fprintf(stdout, "%s%s  %s\n", prefix, h, name)
	}
	return status
}

func (f *fakeSha256sum) args() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []string
	for _, c := range f.calls {
		all = append(all, c[1:]...)
	}
	sort.Strings(all)
	return all
}

var oddNames = []string{"my file.txt", "it's.txt", `a"b.txt`, "-dash.txt", `back\slash.txt`, "$(touch x).txt", "plain.txt"}

func setupVerify(t *testing.T, lie ...string) (*fakeSha256sum, string, Remote) {
	t.Helper()
	f := &fakeSha256sum{lie: map[string]bool{}, release: make(chan struct{})}
	t.Cleanup(func() { close(f.release) })
	for _, l := range lie {
		f.lie[l] = true
	}
	remoteRoot, r, _ := transferEnvCmds(t, map[string]sshtest.Handler{"sha256sum": f.handler})
	f.root = remoteRoot
	for _, n := range oddNames {
		write(t, filepath.Join(remoteRoot, "proj", "run1", "out", n), "content of "+n)
	}
	return f, remoteRoot, r
}

func TestDownloadVerifiesSHA256(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	f, _, r := setupVerify(t)
	var v Verification
	r.OnVerify = func(x Verification) { v = x }
	out := t.TempDir()
	written, err := Download(context.Background(), r, []string{"out"}, out, nil)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if len(written) != len(oddNames) || v.Verified != len(oddNames) || len(v.Mismatched) != 0 || v.SizeOnly != "" {
		t.Errorf("written=%d v=%+v", len(written), v)
	}
	var want []string
	for _, n := range oddNames {
		want = append(want, "proj/run1/out/"+n)
	}
	sort.Strings(want)
	if got := f.args(); !reflect.DeepEqual(got, want) {
		t.Errorf("sha256sum arguments (after quoting round trip) = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(tmpTouched(f.root))); err == nil {
		t.Error("a file name was executed by the remote shell")
	}
}

func tmpTouched(root string) string { return filepath.Join(root, "x") }

func TestDownloadSHA256Mismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	_, _, r := setupVerify(t, "proj/run1/out/my file.txt")
	var v Verification
	r.OnVerify = func(x Verification) { v = x }
	out := t.TempDir()
	written, err := Download(context.Background(), r, []string{"out"}, out, nil)
	if err != nil {
		t.Fatalf("a mismatch must not discard the result: %v", err)
	}
	if len(v.Mismatched) != 1 || !strings.HasSuffix(v.Mismatched[0], "my file.txt.mismatch") {
		t.Fatalf("Mismatched = %v", v.Mismatched)
	}
	if len(written) != len(oddNames)-1 || v.Verified != len(oddNames)-1 || len(v.Mismatched) != 1 {
		t.Errorf("written=%d v=%+v", len(written), v)
	}
	if _, err := os.Stat(filepath.Join(out, "out", "my file.txt.mismatch")); err != nil {
		t.Errorf("mismatching file was not kept as .mismatch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "out", "my file.txt")); err == nil {
		t.Error("the mismatching file still has its normal name")
	}
	for _, w := range written {
		if strings.Contains(w, "my file.txt") {
			t.Errorf("mismatching file listed as written: %s", w)
		}
	}
}

func TestDownloadSizeOnlyFallbacks(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	cases := []struct {
		name string
		cmds map[string]sshtest.Handler
	}{
		{"command not found", map[string]sshtest.Handler{}},
		{"exit 0, empty output (internal-sftp only)", map[string]sshtest.Handler{"sha256sum": func(string, io.Reader, io.Writer, io.Writer) int { return 0 }}},
		{"exit 1, restricted shell", map[string]sshtest.Handler{"sha256sum": func(_ string, _ io.Reader, _ io.Writer, stderr io.Writer) int {
			fmt.Fprintln(stderr, "This account is restricted: commands are not allowed.")
			return 1
		}}},
		{"exit 255", map[string]sshtest.Handler{"sha256sum": func(string, io.Reader, io.Writer, io.Writer) int { return 255 }}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			remoteRoot, r, _ := transferEnvCmds(t, c.cmds)
			write(t, filepath.Join(remoteRoot, "proj", "run1", "a.txt"), "hello")
			var v Verification
			r.OnVerify = func(x Verification) { v = x }
			written, err := Download(context.Background(), r, []string{"a.txt"}, t.TempDir(), nil)
			if err != nil || len(written) != 1 {
				t.Fatalf("Download: %v %v", written, err)
			}
			if !strings.Contains(v.SizeOnly, "sha256sum not available on") || v.Verified != 0 || v.Unverified != 1 || len(v.Failed) != 0 {
				t.Errorf("v = %+v", v)
			}
		})
	}
}

func TestDownloadUnreadableFileIsFailedNotSizeOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	f, remoteRoot, r := setupVerify(t)
	_ = f
	write(t, filepath.Join(remoteRoot, "proj", "run1", "d", "ok.txt"), "ok")
	write(t, filepath.Join(remoteRoot, "proj", "run1", "d", "gone.txt"), "gone")
	// The handler reads files from f.root, so make one unreadable by name.
	f.lie = nil
	f.hide = map[string]bool{"proj/run1/d/gone.txt": true}
	var v Verification
	r.OnVerify = func(x Verification) { v = x }
	written, err := Download(context.Background(), r, []string{"d"}, t.TempDir(), nil)
	if err != nil || len(written) != 2 {
		t.Fatalf("Download: %v %v", written, err)
	}
	if v.SizeOnly != "" || v.Verified != 1 || len(v.Failed) != 1 || v.Failed[0] != "d/gone.txt" {
		t.Errorf("v = %+v", v)
	}
}

func TestDownloadSHA256Batches(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	f, remoteRoot, r := setupVerify(t)
	for i := 0; i < verifyBatchFiles+5; i++ {
		write(t, filepath.Join(remoteRoot, "proj", "run1", "many", fmt.Sprintf("f%03d", i)), "x")
	}
	var v Verification
	r.OnVerify = func(x Verification) { v = x }
	if _, err := Download(context.Background(), r, []string{"many"}, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) < 2 || v.Verified != verifyBatchFiles+5 {
		t.Errorf("calls=%d verified=%d", len(f.calls), v.Verified)
	}
}

func TestDownloadSHA256BatchesByQuotedLength(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	f, remoteRoot, r := setupVerify(t)
	long := strings.Repeat("n", 150)
	for i := 0; i < 130; i++ {
		write(t, filepath.Join(remoteRoot, "proj", "run1", "long", fmt.Sprintf("%s%03d", long, i)), "x")
	}
	var v Verification
	r.OnVerify = func(x Verification) { v = x }
	if _, err := Download(context.Background(), r, []string{"long"}, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) < 2 || v.Verified != 130 {
		t.Errorf("calls=%d verified=%d", len(f.calls), v.Verified)
	}
	for _, c := range f.calls {
		if q := len(strings.Join(c, " ")); q > verifyBatchQuote+1024 {
			t.Errorf("a call had %d bytes of arguments", q)
		}
	}
}

func TestDownloadSHA256TimeoutKeepsEarlierBatches(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	f, remoteRoot, r := setupVerify(t)
	for i := 0; i < verifyBatchFiles+5; i++ {
		write(t, filepath.Join(remoteRoot, "proj", "run1", "many", fmt.Sprintf("f%03d", i)), "x")
	}
	f.blockAfter = 1 // batch 1 answers, batch 2 hangs
	// Batch 1 gets a generous budget so a loaded machine can't time it out;
	// only the hanging batch 2 gets the short one.
	oldTimeout := verifyTimeout
	var nCalls atomic.Int32
	verifyTimeout = func(int64) time.Duration {
		if nCalls.Add(1) == 1 {
			return time.Minute
		}
		return 300 * time.Millisecond
	}
	t.Cleanup(func() { verifyTimeout = oldTimeout })
	var v Verification
	r.OnVerify = func(x Verification) { v = x }
	written, err := Download(context.Background(), r, []string{"many"}, t.TempDir(), nil)
	if err != nil || len(written) != verifyBatchFiles+5 {
		t.Fatalf("Download: %d files, %v", len(written), err)
	}
	if v.Verified != verifyBatchFiles || v.Unverified != 5 || !strings.Contains(v.SizeOnly, "took longer") {
		t.Errorf("v = %+v", v)
	}
}

func TestParseSums(t *testing.T) {
	h := strings.Repeat("a", 64)
	got := parseSums([]byte(h + "  plain name\n" + `\` + h + `  a\\b\nc\rd` + "\n" + h + " *bin\n" + h + "  lead space\nbogus line\n" + h + "x name\n" + strings.Repeat("z", 64) + "  nothex\n"))
	want := map[string]string{"plain name": h, "a\\b\nc\rd": h, "bin": h, "lead space": h}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseSums = %q, want %q", got, want)
	}
}

func TestMarkMismatchNeverOverwrites(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "f")
	write(t, p, "new")
	write(t, p+".mismatch", "old")
	got, ok := markMismatch(p)
	if !ok || got != p+".mismatch2" {
		t.Errorf("markMismatch = %s", got)
	}
	if b, _ := os.ReadFile(p + ".mismatch"); string(b) != "old" {
		t.Error("an earlier .mismatch file was overwritten")
	}
}
