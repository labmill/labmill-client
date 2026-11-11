#!/bin/sh
# check.sh <resultdir> <workdir>: count.txt holds the number of reads
set -eu
want=$(awk 'END { print NR / 4 }' "$2/reads.fastq")
[ "$(tr -d ' \n' < "$1/count.txt")" = "$want" ]
