#!/bin/sh
# check.sh <resultdir> <workdir>: total.txt holds the number of reads
set -eu
want=$(awk 'END { print NR / 4 }' "$2/reads.fastq")
[ "$(tr -d ' \n' < "$1/total.txt")" = "$want" ]
