#!/bin/sh
# check.sh <resultdir> <workdir>: total.txt holds the number of reads
set -eu
want=$(awk 'END { print NR / 4 }' "$2/reads.fastq")
[ "$(tr -d ' \n' < "$1/total.txt")" = "$want" ]
zip=$(ls "$1"/*_fastqc.zip)
[ "$(od -An -tx1 -N2 "$zip" | tr -d ' \n')" = "504b" ]
