#!/bin/sh
# check.sh <resultdir> <workdir>: total.txt holds 1+4+9+16+25
set -eu
[ "$(tr -d ' \n' < "$1/total.txt")" = "55" ]
[ "$(tr -d ' \n' < "$1/part-5.txt")" = "25" ]
