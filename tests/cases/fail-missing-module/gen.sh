#!/bin/sh
set -eu
printf '#!/bin/sh\nmodule load nosuchmodule\necho "done"\n' > "$1/mod.sh"
