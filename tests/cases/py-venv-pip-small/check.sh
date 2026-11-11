#!/bin/sh
# check.sh <resultdir> <workdir>: result.txt holds the package name
set -eu
[ "$(tr -d ' \n' < "$1/result.txt")" = "six" ]
