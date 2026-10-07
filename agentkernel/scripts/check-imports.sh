#!/usr/bin/env bash
# Dependency-closure check: the module may depend only on itself and the
# standard library, for every package including tests, across platforms.
# Source-level checks that also cover build-tagged and ignored files live in
# tests/architecture; this script checks what the Go toolchain resolves.
set -euo pipefail
cd "$(dirname "$0")/.."
export GOWORK=off

module="$(go list -m)"
fail=0

if grep -Eq '^\s*(require|replace)\b' go.mod; then
  echo "imports: go.mod declares require/replace; Gate A admits none without review" >&2
  fail=1
fi

for platform in linux/amd64 darwin/arm64 windows/amd64; do
  export GOOS="${platform%/*}" GOARCH="${platform#*/}"
  while IFS= read -r pkg; do
    [[ -z "$pkg" ]] && continue
    if [[ "$pkg" == "$module" || "$pkg" == "$module"/* ]]; then
      continue
    fi
    first="${pkg%%/*}"
    if [[ "$first" == *.* ]]; then
      echo "imports: $platform: non-stdlib dependency $pkg" >&2
      fail=1
    fi
  done < <(go list -deps -test ./... | sort -u)
done

if [[ $fail -ne 0 ]]; then
  exit 1
fi
echo "imports: ok ($module depends only on itself and the standard library)"
