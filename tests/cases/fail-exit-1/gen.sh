#!/bin/sh
set -eu
printf '#!/bin/sh\necho "starting"\nexit 1\n' > "$1/fail.sh"
