#!/bin/sh
# check.sh <resultdir> <workdir>: hello.txt holds one line for each of the 4 ranks
set -eu
[ "$(grep -E '^rank [0-3] of 4$' "$1/hello.txt" | sort -u | wc -l | tr -d ' ')" = "4" ]
