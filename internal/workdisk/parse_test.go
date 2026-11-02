package workdisk

import "testing"

func TestSizeGB(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
		ok   bool
	}{{"100G", 100, true}, {"500GB", 500, true}, {"1.5T", 1536, true}, {"2048M", 2, true}, {"1TiB", 1024, true}, {"0", 0, true}, {"-", 0, false}, {"", 0, false}} {
		got, ok := sizeGB(c.in, 1)
		if got != c.want || ok != c.ok {
			t.Errorf("%q: %d %v", c.in, got, ok)
		}
	}
}

func TestParseDF(t *testing.T) {
	kib := "Filesystem     Type     1024-blocks      Used Available Capacity Mounted on\n" +
		"tmpfs          tmpfs       65536000         0  65536000       0% /dev/shm\n" +
		"/dev/nvme0n1p1 xfs      1048576000 524288000 524288000      50% /lscratch\n" +
		"srv:/home      nfs4     2097152000 2097152000         0     100% /home dir\n"
	ms := ParseDF(kib)
	if len(ms) != 3 || ms[0].FSType != "tmpfs" || ms[0].FreeGB != 62 || ms[1].SizeGB != 1000 || ms[1].FreeGB != 500 {
		t.Fatalf("%+v", ms)
	}
	if ms[2].Path != "/home dir" || ms[2].FreeGB != 0 {
		t.Fatalf("%+v", ms[2])
	}
	hu := "Filesystem Type Size Used Avail Use% Mounted on\n/dev/sda1 ext4 1.8T 1.0T 700G 60% /tmp\n"
	if ms = ParseDF(hu); len(ms) != 1 || ms[0].SizeGB != 1843 || ms[0].FreeGB != 700 {
		t.Fatalf("%+v", ms)
	}
	for _, bad := range []string{"", "df: /x: No such file or directory\n", "Filesystem Type\n"} {
		if ParseDF(bad) != nil {
			t.Errorf("%q", bad)
		}
	}
}

func TestParseNodes(t *testing.T) {
	out := "NodeName=n1 Arch=x86_64 Gres=gpu:a100:4(S:0-1),lscratch:200 TmpDisk=204800 State=IDLE\n" +
		"NodeName=n2 Gres=(null) TmpDisk=51200\n" +
		"NodeName=n3 Gres=gpu:a100:4(S:0-1) TmpDisk=0\n"
	tmp, g := ParseNodes(out)
	if tmp != 200 || len(g) != 2 || g[0] != "gpu:a100:4" || g[1] != "lscratch:200" {
		t.Fatalf("%d %v", tmp, g)
	}
	if tmp, g = ParseNodes(""); tmp != -1 || g != nil {
		t.Fatalf("empty: %d %v", tmp, g)
	}
	if tmp, _ = ParseNodes("scontrol: error: Invalid user id\n"); tmp != -1 {
		t.Fatal("error output")
	}
}

func TestBuildFacts(t *testing.T) {
	df := "Filesystem Type 1024-blocks Used Available Capacity Mounted on\n" +
		"/dev/a ext4 104857600 1 52428800 50% /\n" +
		"srv:/h nfs 209715200 1 104857600 50% /home\n" +
		"lus lustre 1073741824 1 536870912 50% /scratch\n"
	f := BuildFacts(Probe{TmpDir: "/tmp", Home: "/home/u", SharedRoot: "/scratch/lab", DF: df, HasWorkspace: true, WSMaxDays: 30})
	if f.HomeFreeGB != 100 || f.SharedFreeGB != 512 || len(f.Mounts) != 3 || !f.HasWorkspace || f.WSMaxDays != 30 {
		t.Errorf("facts: %+v", f)
	}
	if got := FreeGB(f.Mounts, "/scratchy/x"); got != 50 {
		t.Errorf("/scratchy must match /, got %d", got)
	}
	e := BuildFacts(Probe{Home: "/home/u", SharedRoot: "/x"})
	if e.HomeFreeGB != -1 || e.SharedFreeGB != -1 || len(e.Mounts) != 0 {
		t.Errorf("empty: %+v", e)
	}
	if FreeGB(f.Mounts, "") != -1 {
		t.Error("empty path must be unknown")
	}
}
