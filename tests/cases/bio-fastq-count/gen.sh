#!/bin/sh
# reads.fastq: 250 reads of 36 bases from a fixed generator (the same bytes on every run)
set -eu
awk 'BEGIN { s = 12345; for (i = 1; i <= 250; i++) { q = ""; r = ""; for (j = 0; j < 36; j++) { s = (s * 16807) % 2147483647; r = r substr("ACGT", int(s / 1024) % 4 + 1, 1); q = q "I" } printf "@read%d\n%s\n+\n%s\n", i, r, q } }' > "$1/reads.fastq"
