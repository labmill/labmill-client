#!/bin/sh
# check.sh <resultdir> <workdir>: totals.csv holds the right sum for each region
set -eu
awk -F, 'NR == FNR { if (FNR > 1) want[$1] += $2; next }
	FNR > 1 { got[$1] = $2; n++ }
	END {
		for (r in want) { d = want[r] - got[r]; if (!(r in got) || d > 0.000001 || d < -0.000001) exit 1 }
		exit (n == 3) ? 0 : 1
	}' "$2/sales.csv" "$1/totals.csv"
