#!/bin/sh
# data.csv: 1000 rows, no random numbers (the same bytes on every run)
set -eu
awk 'BEGIN { for (i = 1; i <= 1000; i++) printf "%d,%d,%d.%02d\n", i, i % 7, (i * 7919) % 1000, (i * 31) % 100 }' > "$1/data.csv"
