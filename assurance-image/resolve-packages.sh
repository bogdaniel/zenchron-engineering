#!/bin/sh
# Regenerates chromium-packages.txt: the exact versions of Chromium and its
# whole dependency closure that debian-snapshot.sources resolves to, computed
# inside the pinned Go base image so the closure is relative to what it already
# has installed. Prints the list on stdout; nothing is written or installed.
#
#   assurance-image/resolve-packages.sh > chromium-packages.txt
#
# Changing the snapshot timestamp, regenerating this list and rebuilding is the
# only way the Chromium package set moves, and it is one reviewed diff.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
go_image=$(sed -n 's/^ARG GO_IMAGE=//p' "$here/Dockerfile")
docker run --rm --entrypoint sh -v "$here/debian-snapshot.sources:/pinned.sources:ro" "$go_image" -c '
set -eu
find /etc/apt/sources.list.d -type f -delete
rm -f /etc/apt/sources.list
cp /pinned.sources /etc/apt/sources.list.d/pinned.sources
echo "Acquire::Check-Valid-Until \"false\";" > /etc/apt/apt.conf.d/99snapshot
apt-get update -qq >/dev/null
apt-get install -s --no-install-recommends chromium fonts-liberation |
  awk "/^Inst /{v=\$3; if (v ~ /^\\[/) v=\$4; sub(/^\\(/,\"\",v); print \$2\"=\"v}" | LC_ALL=C sort
'
