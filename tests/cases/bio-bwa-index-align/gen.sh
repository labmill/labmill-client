#!/bin/sh
# ref.fa: 2000 bases from a fixed generator. reads.fq: 100 reads of 50 bases cut from it.
set -eu
awk -v ref="$1/ref.fa" -v rd="$1/reads.fq" 'BEGIN {
	s = 777; g = ""
	for (i = 0; i < 2000; i++) { s = (s * 16807) % 2147483647; g = g substr("ACGT", int(s / 1024) % 4 + 1, 1) }
	print ">ref" > ref
	for (i = 1; i <= 2000; i += 80) print substr(g, i, 80) > ref
	q = ""; for (j = 0; j < 50; j++) q = q "I"
	for (i = 0; i < 100; i++) printf "@r%d\n%s\n+\n%s\n", i, substr(g, i * 19 + 1, 50), q > rd
}'
