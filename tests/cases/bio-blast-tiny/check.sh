#!/bin/sh
# check.sh <resultdir> <workdir>: hit.txt holds the ID of the sequence the query was cut from
set -eu
[ "$(tr -d ' \n' < "$1/hit.txt")" = "seq2" ]
