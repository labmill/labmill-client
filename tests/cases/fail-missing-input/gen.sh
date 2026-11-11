#!/bin/sh
set -eu
printf '#!/bin/sh\nwc -l < /data/mygroup/nosuchfile.csv\n' > "$1/count.sh"
