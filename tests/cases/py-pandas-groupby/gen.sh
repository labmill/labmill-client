#!/bin/sh
# sales.csv: 600 rows, regions r0 to r2, amount = row number
set -eu
awk 'BEGIN { print "region,amount"; for (i = 1; i <= 600; i++) printf "r%d,%d\n", i % 3, i }' > "$1/sales.csv"
