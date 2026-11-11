#!/bin/sh
# check.sh <resultdir> <workdir>: result.txt holds the package version
set -eu
[ "$(tr -d ' \n' < "$1/result.txt")" = "1.16.0" ]
