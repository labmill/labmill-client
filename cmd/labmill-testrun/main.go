// Command labmill-testrun plays the test corpus (tests/cases) against a
// cluster. This first step has -dry-run only: it loads and validates the
// cases and lists them.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"labmill-client/internal/testcase"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run returns 0 on success, 1 on a failed case (later steps), 2 on a usage
// error or an invalid case.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("labmill-testrun", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "tests/cases", "folder with the case folders")
	glob := fs.String("cases", "", "keep cases whose id matches this glob")
	tags := fs.String("tags", "", "keep cases with all these tags (comma separated)")
	dry := fs.Bool("dry-run", false, "list the valid cases and stop")
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
	if !*dry {
		fmt.Fprintln(stderr, "running cases is not built yet: use -dry-run")
		return 2
	}
	return 0
}
