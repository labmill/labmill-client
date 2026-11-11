#!/bin/sh
# check.sh <resultdir> <workdir>: plot.png is a PNG file of more than 1000 bytes
set -eu
[ "$(od -An -tx1 -N8 "$1/plot.png" | tr -d ' \n')" = "89504e470d0a1a0a" ]
[ "$(wc -c < "$1/plot.png")" -gt 1000 ]
