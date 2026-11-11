#!/bin/sh
# db.fa: 3 sequences of 300 bases from a fixed generator. query.fa: bases 101 to 200 of seq2.
set -eu
awk -v db="$1/db.fa" -v qf="$1/query.fa" 'BEGIN {
	s = 4242
	for (k = 1; k <= 3; k++) {
		g = ""
		for (i = 0; i < 300; i++) { s = (s * 16807) % 2147483647; g = g substr("ACGT", int(s / 1024) % 4 + 1, 1) }
		print ">seq" k > db
		print g > db
		if (k == 2) { print ">query" > qf; print substr(g, 101, 100) > qf }
	}
}'
