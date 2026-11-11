#!/bin/sh
# small.sam: 20 alignments on one reference, in reverse order of position
set -eu
awk 'BEGIN {
	print "@HD\tVN:1.6\tSO:unsorted"
	print "@SQ\tSN:chr1\tLN:1000"
	for (i = 20; i >= 1; i--) printf "read%d\t0\tchr1\t%d\t60\t10M\t*\t0\t0\tACGTACGTAC\tIIIIIIIIII\n", i, i * 30
}' > "$1/small.sam"
