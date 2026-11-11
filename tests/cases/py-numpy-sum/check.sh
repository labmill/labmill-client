#!/bin/sh
# check.sh <resultdir> <workdir>: result.txt holds the sum of column 3 of data.csv
set -eu
want=$(awk -F, '{ s += $3 } END { printf "%.2f", s }' "$2/data.csv")
got=$(tr -d ' \n' < "$1/result.txt")
awk -v a="$want" -v b="$got" 'BEGIN { d = a - b; if (d < 0) d = -d; exit (b != "" && d < 0.000001) ? 0 : 1 }'
