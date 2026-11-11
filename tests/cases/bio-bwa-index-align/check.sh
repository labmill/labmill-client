#!/bin/sh
# check.sh <resultdir> <workdir>: aligned.bam is a BAM file and at least 95 of the 100 reads are mapped
set -eu
[ "$(od -An -tx1 -N2 "$1/aligned.bam" | tr -d ' \n')" = "1f8b" ]
[ "$(tr -d ' \n' < "$1/mapped.txt")" -ge 95 ]
