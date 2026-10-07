#!/usr/bin/env bash
# T0 static evidence (#516): cheap, deterministic, run on every pull request.
# The full `go` job runs this same script, so the list exists once.
set -euo pipefail

unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  echo "gofmt needed:"; echo "$unformatted"; exit 1
fi

go vet ./...

# A build-tagged file that no CI job compiles rots invisibly:
# control_endpoint_other.go referenced an undefined identifier and both Linux
# and macOS jobs stayed green, because neither ever selects that build tag.
# Compiling for a non-unix target is what makes the refusal path a checked one.
GOOS=windows GOARCH=amd64 go build ./...

# The build above never compiles _test.go files; vet does, so a test leaning on
# a Unix-only helper cannot slip in untagged.
GOOS=windows GOARCH=amd64 go vet ./...
