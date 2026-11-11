#!/bin/sh
set -eu
printf '#!/bin/sh\necho "hello"\n' > "$1/hello.sh"
