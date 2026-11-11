#!/bin/sh
# check.sh <resultdir> <workdir>: f-1.txt to f-200.txt are there and hold their numbers
set -eu
n=1
while [ "$n" -le 200 ]; do
	[ "$(tr -d ' \n' < "$1/f-$n.txt")" = "$n" ]
	n=$((n + 1))
done
