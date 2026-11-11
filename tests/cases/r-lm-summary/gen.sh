#!/bin/sh
# xy.csv: y = 2x + 1 with a fixed small wobble
set -eu
awk 'BEGIN { print "x,y"; for (i = 1; i <= 50; i++) printf "%d,%.1f\n", i, 2 * i + 1 + ((i * 7) % 5 - 2) / 10 }' > "$1/xy.csv"
