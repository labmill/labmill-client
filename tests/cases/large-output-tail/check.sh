#!/bin/sh
# check.sh <resultdir> <workdir>: last.txt holds the last number
set -eu
[ "$(tr -d ' \n' < "$1/last.txt")" = "100000" ]
