package launch

import (
	"strings"
	"testing"

	"labmill-client/internal/store"
)

func TestOwnFrames(t *testing.T) {
	stack := "goroutine 7 [running]:\n" +
		"runtime/debug.Stack()\n\t/usr/local/go/src/runtime/debug/stack.go:26 +0x5e\n" +
		"labmill-client/internal/app.(*toolset).run(0xc000123, {0x1})\n\t/home/someone/src/labmill/internal/app/tools.go:120 +0x1f\n" +
		"github.com/x/y.F()\n\t/home/someone/go/pkg/mod/x/y.go:9 +0x2\n" +
		"labmill-client/internal/launch.(*Session).Send(...)\n\t/home/someone/src/labmill/internal/launch/session.go:40 +0x3\n"
	got := ownFrames(stack)
	want := "labmill-client/internal/app.(*toolset).run tools.go:120\nlabmill-client/internal/launch.(*Session).Send session.go:40"
	if got != want {
		t.Errorf("ownFrames = %q, want %q", got, want)
	}
	if strings.Contains(got, "someone") || strings.Contains(got, "0x") {
		t.Errorf("frames hold a folder or an argument: %q", got)
	}
}

func TestReportWordsHasJobFolders(t *testing.T) {
	w := strings.Join(reportWords(nil, store.Job{RemoteDir: "/data/mygroup/private-project", LocalDir: "/home/x/labmill-results/run7"}), " ")
	for _, want := range []string{"mygroup", "private-project", "run7"} {
		if !strings.Contains(w, want) {
			t.Errorf("words %q lack %s", w, want)
		}
	}
}
