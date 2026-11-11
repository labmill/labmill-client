#!/bin/sh
# check.sh <resultdir> <workdir>: gpus.txt lists exactly one GPU
set -eu
[ "$(grep -c '^GPU 0:' "$1/gpus.txt")" = "1" ]
! grep -q '^GPU 1:' "$1/gpus.txt"
