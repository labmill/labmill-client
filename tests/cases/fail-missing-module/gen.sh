#!/bin/sh
set -eu
printf '#!/bin/sh\nmodule load nosuchmodule\nnosuchtool --version\n' > "$1/mod.sh"
