#!/bin/sh
set -eu
printf '#!/bin/sh\ncat /data/mygroup/nosuchfile.csv | wc -l\n' > "$1/count.sh"
