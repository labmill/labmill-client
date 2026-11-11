#!/bin/sh
# check.sh <resultdir> <workdir>: total.txt holds the sum of the squares 1 to n
set -eu
want=$(awk -v n="$(cat "$2/n.txt")" 'BEGIN { printf "%.0f", n * (n + 1) * (2 * n + 1) / 6 }')
got=$(tr -d ' \n' < "$1/total.txt")
[ "$got" = "$want" ]
