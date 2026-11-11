#!/bin/sh
# check.sh <resultdir> <workdir>: the program ran with 4 threads
set -eu
grep -qx 'threads 4' "$1/threads.txt"
