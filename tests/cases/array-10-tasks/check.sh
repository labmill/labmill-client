#!/bin/sh
# check.sh <resultdir> <workdir>: out-1.txt to out-10.txt hold N*N
set -eu
n=1
while [ "$n" -le 10 ]; do
	[ "$(tr -d ' \n' < "$1/out-$n.txt")" = "$((n * n))" ]
	n=$((n + 1))
done
