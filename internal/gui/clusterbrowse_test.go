package gui

import (
	"context"
	"io"
	"strings"
	"testing"

	"labmill-client/internal/backend"
	"labmill-client/internal/config"
)

type browseRunner struct{ out string }

func (r browseRunner) Run(context.Context, []string, io.Reader) (backend.Result, error) {
	return backend.Result{Stdout: r.out}, nil
}

func TestBrowseCluster(t *testing.T) {
	f := newFolderFixture(t)
	f.env.Config = &config.Config{Clusters: []config.Cluster{{Name: "bigcluster"}}}
	f.env.LoginRunner = func(context.Context, config.Cluster) (backend.Runner, error) {
		return browseRunner{"@@PWD\n/data/mygroup\n@@FREE\n100\n@@WRITE\nyes\n@@DIRS\nrun1/\nrun2/\n"}, nil
	}
	names, err := f.app.ClusterNames()
	if err != nil || len(names) != 1 || names[0] != "bigcluster" {
		t.Fatalf("names %v, %v", names, err)
	}
	cf, err := f.app.BrowseCluster("bigcluster", "/data/mygroup")
	if err != nil || cf.Path != "/data/mygroup" || !cf.Writable || strings.Join(cf.Dirs, ",") != "run1,run2" {
		t.Fatalf("browse %+v, %v", cf, err)
	}
	if _, err := f.app.BrowseCluster("nope", ""); err == nil {
		t.Error("unknown cluster must fail")
	}
}
