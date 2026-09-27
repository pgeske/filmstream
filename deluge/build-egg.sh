#!/bin/sh
# Builds the TeaStream Deluge plugin egg inside the Deluge image it will run
# in, so the egg carries that image's Python version tag (Deluge only loads
# eggs built for its own Python).
#
# Usage: deluge/build-egg.sh [OUTPUT_DIR]
#   OUTPUT_DIR defaults to deluge/dist. DELUGE_IMAGE overrides the image
#   (default lscr.io/linuxserver/deluge:latest).
set -eu

image=${DELUGE_IMAGE:-lscr.io/linuxserver/deluge:latest}
here=$(cd "$(dirname "$0")" && pwd)
out=${1:-"$here/dist"}
mkdir -p "$out"
out=$(cd "$out" && pwd)

docker run --rm \
	--user "$(id -u):$(id -g)" \
	--env HOME=/tmp \
	--env PYTHONWARNINGS=ignore \
	--entrypoint sh \
	--volume "$here/teastream:/src:ro" \
	--volume "$out:/out" \
	"$image" -c '
set -eu
cp -R /src /tmp/teastream
cd /tmp/teastream
python3 setup.py --quiet bdist_egg --dist-dir /out >/dev/null
'

ls "$out"/TeaStream-*.egg
