#!/bin/sh
# check.sh <resultdir> <workdir>: sum.txt starts with the SHA-256 of the local data.bin
set -eu
if command -v sha256sum > /dev/null; then want=$(sha256sum "$2/data.bin"); else want=$(shasum -a 256 "$2/data.bin"); fi
want=${want%% *}
got=$(awk '{ print $1; exit }' "$1/sum.txt")
[ -n "$got" ] && [ "$got" = "$want" ]
