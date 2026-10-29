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
  # Capture first: a process substitution's exit status is invisible to
  # pipefail, and an unresolvable import (a parent-module path) is exactly
  # what makes go list fail.
  if ! deps="$(go list -deps -test ./... 2>&1)"; then
    echo "imports: $platform: go list failed:" >&2
    echo "$deps" >&2
    fail=1
    continue
  fi
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
  done < <(sort -u <<<"$deps")
done

if [[ $fail -ne 0 ]]; then
  exit 1
fi
echo "imports: ok ($module depends only on itself and the standard library)"
