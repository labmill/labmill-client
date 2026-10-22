package app

import (
	"strings"
	"testing"

	"labmill-client/internal/rules"
)

func TestDraftEnvironment(t *testing.T) {
	h, _ := slurmHarness(t, rules.SlurmHints{SoftwareDir: "/sw/lab"})
	env := map[string]any{
		"modules": []string{"python/3.11"},
		"venv":    map[string]any{"requirements": "requirements.txt", "python": "python3.11"},
		// The model can't choose the folder: the profile does.
		"SoftwareDir": "/tmp/evil",
	}
	out, err := h.run(t, "draft_job", draftSpec(map[string]any{"environment": env}, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"module load python/3.11 || exit 1", `${LABMILL_ENVS:-/sw/lab}`, "labmill_once", "python train.py"} {
		if !strings.Contains(out, want) {
			t.Errorf("draft lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "/tmp/evil") {
		t.Errorf("the model chose the software folder:\n%s", out)
	}
}

func TestDraftEnvironmentRefusals(t *testing.T) {
	h, _ := slurmHarness(t, rules.SlurmHints{})
	for name, env := range map[string]map[string]any{
		"dotdot": {"venv": map[string]any{"requirements": "../r.txt"}},
		"two":    {"venv": map[string]any{"requirements": "r.txt"}, "conda": map[string]any{"file": "e.yml"}},
		"module": {"modules": []string{"a b"}},
		"image":  {"apptainer": map[string]any{"image": "docker://x y"}},
	} {
		if _, err := h.run(t, "draft_job", draftSpec(map[string]any{"environment": env}, nil)); err == nil || !strings.Contains(err.Error(), "isn't valid") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
