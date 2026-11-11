#!/bin/sh
# check.sh <resultdir> <workdir>: slope.txt holds the least squares slope, within 0.01
set -eu
want=$(awk -F, 'NR > 1 { n++; sx += $1; sy += $2; sxy += $1 * $2; sxx += $1 * $1 } END { printf "%.6f", (n * sxy - sx * sy) / (n * sxx - sx * sx) }' "$2/xy.csv")
got=$(tr -d ' \n' < "$1/slope.txt")
awk -v a="$want" -v b="$got" 'BEGIN { d = a - b; if (d < 0) d = -d; exit (b != "" && d < 0.01) ? 0 : 1 }'
