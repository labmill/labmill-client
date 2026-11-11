#!/bin/sh
set -eu
printf '#!/bin/sh\nsleep 100\necho finished\n' > "$1/sleep.sh"
