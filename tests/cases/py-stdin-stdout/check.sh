#!/bin/sh
# check.sh <resultdir> <workdir>
set -eu
want=$(wc -l < "$2/lines.txt" | tr -d ' ')
got=$(tr -d ' \n' < "$1/count.txt")
[ "$want" = "$got" ]
