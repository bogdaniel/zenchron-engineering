#!/bin/sh
# Regenerates chromium-packages.txt: the exact versions of Chromium and its
# whole dependency closure that debian-snapshot.sources resolves to, computed
# inside the pinned Go base image so the closure is relative to what it already
# has installed. Prints the list on stdout and exits non-zero, printing nothing, if
# the simulation fails; nothing is written or installed.
#
#   assurance-image/resolve-packages.sh > resolved.txt && mv resolved.txt assurance-image/chromium-packages.txt
#
# Changing the snapshot timestamp, regenerating this list and rebuilding is the
# only way the Chromium package set moves, and it is one reviewed diff.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
go_image=$(sed -n 's/^ARG GO_IMAGE=//p' "$here/Dockerfile")
docker run --rm --entrypoint sh -v "$here:/pin:ro" "$go_image" -c '
set -eu
find /etc/apt/sources.list.d -type f -delete
rm -f /etc/apt/sources.list
cp /pin/debian-snapshot.sources /etc/apt/sources.list.d/pinned.sources
echo "Acquire::Check-Valid-Until \"false\";" > /etc/apt/apt.conf.d/99snapshot
apt-get update -qq >/dev/null
/pin/simulate-install.sh
'
