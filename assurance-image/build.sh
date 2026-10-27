#!/bin/sh
# Builds the browser-capable assurance image locally and records what was built.
#
#   assurance-image/build.sh <tag> [record.json]
#
# It never retags or removes an existing image, never touches the configured
# assurance.image, and never prunes. The record names the immutable image ID and
# the facts a reviewer needs to reproduce or distrust it.
set -eu
tag=${1:?usage: build.sh <tag> [record.json]}
record=${2:-"$tag.record.json"}
here=$(cd "$(dirname "$0")" && pwd)

! docker image inspect "$tag" >/dev/null 2>&1 || { echo "$tag already exists; choose a new tag, an existing image is never retagged" >&2; exit 1; }

# No attestation manifests: the image is a single manifest whose ID is what
# assurance.image pins; the inputs are recorded in the file written below.
docker build --pull --provenance=false --sbom=false --tag "$tag" "$here"
id=$(docker image inspect --format '{{.Id}}' "$tag")

probe() { docker run --rm --network none --entrypoint sh "$id" -c "$1"; }
{
  printf '{\n'
  printf '  "image_id": "%s",\n' "$id"
  printf '  "tag": "%s",\n' "$tag"
  printf '  "architecture": "%s",\n' "$(docker image inspect --format '{{.Architecture}}' "$id")"
  printf '  "go": "%s",\n' "$(probe 'go version')"
  printf '  "node": "%s",\n' "$(probe 'node -v')"
  printf '  "npm": "%s",\n' "$(probe 'npm -v')"
  printf '  "chromium": "%s",\n' "$(probe '/opt/zenchron-browser/chrome/chrome --version')"
  printf '  "playwright_core": "%s",\n' "$(probe 'node -p "require(\"/opt/zenchron-browser/playwright/node_modules/playwright-core/package.json\").version"')"
  printf '  "lockfile_sha256": "%s",\n' "$(shasum -a 256 "$here/package-lock.json" | cut -d' ' -f1)"
  printf '  "packages_sha256": "%s",\n' "$(shasum -a 256 "$here/chromium-packages.txt" | cut -d' ' -f1)"
  printf '  "snapshot_sha256": "%s",\n' "$(shasum -a 256 "$here/debian-snapshot.sources" | cut -d' ' -f1)"
  printf '  "dockerfile_sha256": "%s"\n' "$(shasum -a 256 "$here/Dockerfile" | cut -d' ' -f1)"
  printf '}\n'
} > "$record"
echo "image $id recorded in $record"
