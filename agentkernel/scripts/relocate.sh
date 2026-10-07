#!/usr/bin/env bash
# Relocation proof: in a temporary copy outside the repository, rewrite only
# the module's own import prefix to a synthetic module path and build/test.
# A pass shows the current module path is not an accidental dependency on the
# host repository. The synthetic module is never published.
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
repo="$(git -C "$here" rev-parse --show-toplevel)"
dest="$(mktemp -d "${TMPDIR:-/tmp}/agentkernel-relocate.XXXXXX")"
trap 'rm -rf "$dest"' EXIT

old="$(cd "$here" && GOWORK=off go list -m)"
new="example.invalid/relocated/agentkernel"

(cd "$repo" && git ls-files -co --exclude-standard -- agentkernel) |
  while IFS= read -r f; do
    # A tracked file deleted in the working tree is not part of the module.
    [[ -e "$repo/$f" || -L "$repo/$f" ]] || continue
    rel="${f#agentkernel/}"
    mkdir -p "$dest/$(dirname "$rel")"
    cp -P "$repo/$f" "$dest/$rel"
  done

cd "$dest"
# Rewrite the module line and quoted import paths with the exact prefix only.
find . -name '*.go' -o -name go.mod | while IFS= read -r f; do
  perl -pi -e "s#\Q\"$old\E([/\"])#\"$new\$1#g; s#^module \Q$old\E\$#module $new#" "$f"
done

if grep -rIl --include='*.go' --include=go.mod -F "\"$old" . >/dev/null; then
  echo "relocate: stale references to $old remain" >&2
  exit 1
fi

export GOWORK=off GOFLAGS=-mod=readonly
[[ "$(go list -m)" == "$new" ]]
go build ./...
go vet ./...
go test -count=1 ./...
echo "relocate: ok (built and tested as $new)"
