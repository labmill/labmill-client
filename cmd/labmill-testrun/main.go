// Command labmill-testrun plays the test corpus (tests/cases) against a
// cluster and can send the results to the Labmill account (-report).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"labmill-client/internal/account"
	"labmill-client/internal/config"
	"labmill-client/internal/launch"
	"labmill-client/internal/testcase"
	"labmill-client/internal/testrun"
)

func main() {
	// sshx and rclone re-run this executable as the SSH helper and transport.
	if launch.HelperSubcommand(os.Args[1:]) {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		code := launch.RunSubcommand(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
		stop()
		os.Exit(code)
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run returns 0 on success, 1 on a failed case (later steps), 2 on a usage
// error or an invalid case.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("labmill-testrun", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "tests/cases", "folder with the case folders")
	glob := fs.String("cases", "", "keep cases whose id matches this glob")
	tags := fs.String("tags", "", "keep cases with all these tags (comma separated)")
	dry := fs.Bool("dry-run", false, "list the valid cases and stop")
	cluster := fs.String("cluster", "", "alias of the cluster to run on")
	have := fs.String("has", "", "capabilities the cluster has, comma separated (gpu, mpi)")
	capUSD := fs.Float64("cap", 2, "cost cap in USD for the whole run")
	report := fs.Bool("report", false, "send the results to the Labmill account")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cases, errs := testcase.Load(os.DirFS(*dir), ".")
	for _, e := range errs {
		fmt.Fprintln(stderr, "invalid case:", e)
	}
	if len(errs) > 0 {
		return 2
	}
	var want []string
	if *tags != "" {
		for _, t := range strings.Split(*tags, ",") {
			if t = strings.TrimSpace(t); t != "" {
				want = append(want, t)
			}
		}
	}
	cases, err := testcase.Filter(cases, *glob, want)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	for _, c := range cases {
		fmt.Fprintf(stdout, "%-28s %-10s %4ds  %s\n", c.ID, c.Expect.Outcome, c.TimeBudget, c.Title)
	}
	fmt.Fprintf(stdout, "%d cases\n", len(cases))
	if *dry {
		return 0
	}
	if *cluster == "" {
		fmt.Fprintln(stderr, "-cluster is required to run cases")
		return 2
	}
	return play(cases, *dir, *cluster, *have, *capUSD, *report, stdout, stderr)
}

func play(cases []testcase.Case, dir, cluster, have string, capUSD float64, report bool, stdout, stderr io.Writer) int {
	cfgPath, err := config.DefaultPath()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	home := os.Getenv("LABMILL_TESTRUN_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		home = filepath.Join(h, "labmill-testrun")
	}
	if abs, err := filepath.Abs(home); err == nil {
		home = abs // a relative XDG folder would be ignored by the job store
	}
	if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("LABMILL_BROKER_URL") == "" {
		fmt.Fprintln(stderr, "warning: no ANTHROPIC_API_KEY or broker set; the run only works with a site broker")
	}
	started := time.Now().UTC()
	ref := "run-" + started.Format("20060102T150405Z")
	runDir := filepath.Join(home, ref)
	caps := map[string]bool{}
	for _, c := range strings.Split(have, ",") {
		caps[strings.TrimSpace(c)] = true
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	// Read the account link and the salt before the driver moves the XDG folders.
	var link account.Link
	var linked bool
	if report {
		if ap, err := account.Path(); err == nil {
			link, linked, _ = account.Load(ap)
		}
		if !linked {
			fmt.Fprintln(stderr, "-report needs a linked account: run labmill login")
			return 2
		}
	}
	salt, err := testrun.Salt(filepath.Join(filepath.Dir(cfgPath), "test-salt"), randomSalt)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	d := &testrun.Driver{Opt: testrun.Options{Cluster: cluster, CapUSD: capUSD, ConfigIn: cfgPath, CasesDir: absDir}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	results := testrun.RunAll(ctx, d, cases, runDir, caps, time.Now)
	fmt.Fprint(stdout, testrun.Table(results))
	rep := testrun.Report{RunRef: ref, ClientVersion: launch.Version, ClusterHash: testrun.ClusterHash(salt, cluster),
		Scheduler: "slurm", StartedAt: started, Results: results}
	if b, err := json.MarshalIndent(rep, "", "  "); err == nil {
		_ = os.MkdirAll(runDir, 0o755)
		_ = os.WriteFile(filepath.Join(runDir, "report.json"), b, 0o644)
	}
	pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer pcancel()
	if report {
		if err := testrun.Post(pctx, &http.Client{Timeout: 30 * time.Second}, link.URL, link.Token, rep); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	return testrun.ExitCode(results)
}

func randomSalt() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
