#!/usr/bin/env bash
# Extraction proof: copy only the module's own files into a temporary
# directory outside the repository and build, vet and test there with
# GOWORK=off. The real go.mod is copied unchanged, so an undeclared
# dependency cannot be hidden by a generated replacement.
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
repo="$(git -C "$here" rev-parse --show-toplevel)"
dest="$(mktemp -d "${TMPDIR:-/tmp}/agentkernel-extract.XXXXXX")"
trap 'rm -rf "$dest"' EXIT

case "$dest" in
  "$repo"/*) echo "extract: temporary directory is inside the repository" >&2; exit 2 ;;
esac

# Tracked and untracked-but-not-ignored module files; nothing else.
(cd "$repo" && git ls-files -co --exclude-standard -- agentkernel) |
  while IFS= read -r f; do
    rel="${f#agentkernel/}"
    mkdir -p "$dest/$(dirname "$rel")"
    cp -P "$repo/$f" "$dest/$rel"
  done

cmp -s "$here/go.mod" "$dest/go.mod" || { echo "extract: go.mod differs" >&2; exit 1; }

cd "$dest"
export GOWORK=off GOFLAGS=-mod=readonly
go build ./...
go vet ./...
go test -count=1 ./...
echo "extract: ok ($(go list -m) built, vetted and tested in $dest)"
