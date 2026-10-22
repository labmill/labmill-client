package app

import (
	"context"
	"strings"
	"testing"
)

// A sample in a OneDrive folder (spaces, "OneDrive - Example University")
// stages with its real file names, and a job name made from such a folder
// name is answered with a valid suggestion (M38.4).
func TestStageOneDriveFolderWithSpaces(t *testing.T) {
	h := newHarness(t)
	h.write(t, "OneDrive - Example University/Desktop/My Sample/README.md", "# build\n")
	h.write(t, "OneDrive - Example University/Desktop/My Sample/test data/small file.in", "1 2 3\n")
	h.write(t, "OneDrive - Example University/Desktop/My Sample/CMakeLists.txt", "project(x)\n")
	root := h.home + "/OneDrive - Example University/Desktop/My Sample"

	out, err := h.run(t, "stage_data", map[string]any{"root": root, "paths": []string{"."}, "job_name": "my-sample"})
	if err != nil {
		t.Fatalf("stage_data: %v", err)
	}
	if !strings.Contains(out, `"files":3`) {
		t.Errorf("stage result = %s", out)
	}
	m, _ := h.env.Store.Manifest(context.Background(), 1)
	got := map[string]bool{}
	for _, e := range m {
		got[e.Path] = true
	}
	for _, want := range []string{"README.md", "CMakeLists.txt", "test data/small file.in"} {
		if !got[want] {
			t.Errorf("manifest lacks %q: %v", want, got)
		}
	}

	_, err = h.run(t, "stage_data", map[string]any{"root": root, "paths": []string{"."}, "job_name": "OneDrive - Example University"})
	if err == nil || !strings.Contains(err.Error(), "OneDrive-Example-University") {
		t.Errorf("job name from a folder name: %v", err)
	}
}

func TestSafeJobName(t *testing.T) {
	for in, want := range map[string]string{
		"OneDrive - Example University":      "OneDrive-Example-University",
		`C:\Users\Ann Lee\OneDrive - X\spai`: "C-Users-Ann-Lee-OneDrive-X-spai",
		"  ..hidden  ":                       "hidden",
		"Ünï cødé":                           "n-c-d",
		"":                                   "job",
		"***":                                "job",
		strings.Repeat("a", 100):             strings.Repeat("a", 64),
	} {
		got := safeJobName(in)
		if got != want {
			t.Errorf("safeJobName(%q) = %q, want %q", in, got, want)
		}
		if !jobNameRe.MatchString(got) {
			t.Errorf("safeJobName(%q) = %q isn't a valid job name", in, got)
		}
	}
}
