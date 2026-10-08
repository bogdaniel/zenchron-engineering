#!/usr/bin/env bash
# Gate A scope check for #446: every path this feature lane changes must be
# inside the write allowlist. It compares the branch against its merge-base
# with BASE_REF (default origin/main), counts both sides of renames, deletions
# and type changes, and (locally) untracked and uncommitted files too.
#
# The allowlist below is the #446 contract. Broadening it from the change it
# checks requires independent review; this script enforces engineering
# discipline, not isolation from a malicious same-user process.
set -euo pipefail

BASE_REF="${BASE_REF:-origin/main}"
ALLOW_DIR="agentkernel/"
ALLOW_FILE=".github/workflows/agent-kernel.yml"

cd "$(git rev-parse --show-toplevel)"
base="$(git merge-base "$BASE_REF" HEAD 2>/dev/null)" || {
  echo "scope: cannot establish merge-base with $BASE_REF; refusing to pass" >&2
  exit 2
}

allowed() {
  [[ "$1" == "$ALLOW_FILE" || "$1" == "$ALLOW_DIR"* ]]
}

fail=0
check() {
  local p="$1"
  if ! allowed "$p"; then
    echo "scope: out-of-allowlist path: $p" >&2
    fail=1
  fi
}

# Committed changes: --name-status -M prints both sides of a rename.
while IFS=$'\t' read -r status a b; do
  [[ -z "${status:-}" ]] && continue
  check "$a"
  [[ -n "${b:-}" ]] && check "$b"
done < <(git diff --name-status -M "$base" HEAD)

# Local working tree: staged, unstaged and untracked (non-ignored) paths.
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  p="${line:3}"
  if [[ "$p" == *" -> "* ]]; then
    check "${p%% -> *}"
    p="${p##* -> }"
  fi
  check "$p"
done < <(git status --porcelain=v1 --untracked-files=all)

# Symlinks inside the module must not point outside it.
while IFS= read -r link; do
  target="$(cd "$(dirname "$link")" && realpath -q "$(readlink "$link")" 2>/dev/null || true)"
  module="$(realpath "$ALLOW_DIR")"
  if [[ -z "$target" || "$target" != "$module"/* ]]; then
    echo "scope: symlink escapes the module: $link -> $(readlink "$link")" >&2
    fail=1
  fi
done < <(find "$ALLOW_DIR" -type l 2>/dev/null)

if [[ $fail -ne 0 ]]; then
  exit 1
fi
echo "scope: ok (base $base)"
