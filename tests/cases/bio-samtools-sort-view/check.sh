#!/bin/sh
# check.sh <resultdir> <workdir>: sorted.bam is a BAM file and count.txt holds the number of alignments
set -eu
[ "$(od -An -tx1 -N2 "$1/sorted.bam" | tr -d ' \n')" = "1f8b" ]
want=$(grep -vc '^@' "$2/small.sam")
[ "$(tr -d ' \n' < "$1/count.txt")" = "$want" ]
