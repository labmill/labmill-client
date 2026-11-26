package app

import (
	"strings"
	"testing"

	"labmill-client/internal/rules"
)

func TestPurgeNote(t *testing.T) {
	h := rules.SlurmHints{GlobalScratch: &rules.GlobalScratch{Path: "/scratch/<user>", PurgeDays: 30}}
	if got := purgeNote(h, "/scratch/alice/labmill/x"); !strings.Contains(got, "purged after 30 days") {
		t.Errorf("under a purged scratch: %q", got)
	}
	for name, c := range map[string]struct {
		h   rules.SlurmHints
		dir string
	}{
		"outside":       {h, "/home/alice/labmill/x"},
		"purge unknown": {rules.SlurmHints{GlobalScratch: &rules.GlobalScratch{Path: "/scratch/<user>"}}, "/scratch/alice/x"},
		"no profile":    {rules.SlurmHints{}, "/scratch/alice/x"},
	} {
		if got := purgeNote(c.h, c.dir); got != "" {
			t.Errorf("%s: %q", name, got)
		}
	}
}
