#!/bin/sh
# Runs inside the resolver container, after the pinned sources are in place:
# simulates installing Chromium and prints "package=version" for everything it
# would install, byte-sorted. A failed simulation or an empty or incomplete
# result is an error, never a short list.
set -eu
simulation=$(apt-get install -s --no-install-recommends chromium fonts-liberation)
packages=$(printf '%s\n' "$simulation" |
  awk '/^Inst /{v=$3; if (v ~ /^\[/) v=$4; sub(/^\(/,"",v); print $2"="v}' | LC_ALL=C sort)
printf '%s\n' "$packages" | grep -q '^chromium=' || { echo "the simulation did not resolve chromium" >&2; exit 1; }
printf '%s\n' "$packages"
