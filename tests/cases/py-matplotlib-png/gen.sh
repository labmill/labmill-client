#!/bin/sh
# points.csv: x and x squared for x from 0 to 20
set -eu
awk 'BEGIN { print "x,y"; for (i = 0; i <= 20; i++) printf "%d,%d\n", i, i * i }' > "$1/points.csv"
