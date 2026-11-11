#!/bin/sh
# data.bin: 50000 lines of numbers (about 290 KB)
set -eu
awk 'BEGIN { for (i = 1; i <= 50000; i++) printf "%d\n", i * 7919 % 100003 }' > "$1/data.bin"
