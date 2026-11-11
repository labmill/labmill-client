#!/bin/sh
set -eu
awk 'BEGIN { for (i = 1; i <= 500; i++) printf "line %d\n", i }' > "$1/lines.txt"
