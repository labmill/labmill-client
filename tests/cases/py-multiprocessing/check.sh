#!/bin/sh
# check.sh <resultdir> <workdir>: count.txt holds the number of primes below the limit
set -eu
want=$(awk -v n="$(cat "$2/limit.txt")" 'BEGIN { for (i = 2; i < n; i++) c[i] = 1; for (i = 2; i * i < n; i++) if (c[i]) for (j = i * i; j < n; j += i) c[j] = 0; for (i = 2; i < n; i++) k += c[i]; print k }')
[ "$(tr -d ' \n' < "$1/count.txt")" = "$want" ]
