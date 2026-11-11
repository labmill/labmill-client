#!/bin/sh
set -eu
printf 'data = bytearray(2 * 1024 * 1024 * 1024)\nfor i in range(0, len(data), 4096):\n    data[i] = 1\nprint("done")\n' > "$1/oom.py"
