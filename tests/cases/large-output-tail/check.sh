#!/bin/sh
# check.sh <resultdir> <workdir>: last.txt holds the sum
set -eu
[ "$(tr -d ' \n' < "$1/last.txt")" = "18000003000000" ]
