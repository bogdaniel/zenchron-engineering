#!/bin/sh
# Checks a built image against what the runtime assumes, inside the same
# restrictions the verifier applies: no network, read-only root, all
# capabilities dropped, no-new-privileges.
#
#   assurance-image/verify.sh <image> [<reference-image>]
#
# With a reference image (the one in service today) it also shows the Go
# environment is unchanged. Exit status is non-zero on any difference.
set -eu
image=${1:?usage: verify.sh <image> [<reference-image>]}
reference=${2:-}
here=$(cd "$(dirname "$0")" && pwd)
confined() { docker run --rm --network none --read-only --cap-drop ALL --security-opt no-new-privileges --tmpfs /tmp:rw,nosuid,nodev,size=64m -v "$here/chromium-packages.txt:/expected-packages.txt:ro" --entrypoint sh "$@"; }

confined "$image" -c '
set -eu
test -f /opt/zenchron-browser/playwright/index.mjs
test -x /opt/zenchron-browser/chrome/chrome
major=$(node -p "process.versions.node.split(\".\")[0]")
test "$major" -ge 22 || { echo "node $major is older than 22"; exit 1; }
for tool in tar base64 sha256sum awk find sort go gofmt; do command -v $tool >/dev/null || { echo "missing $tool"; exit 1; }; done
find --version | head -1 | grep -q "GNU findutils"
tar --version | head -1 | grep -q "GNU tar"
test "$(go env GOTOOLCHAIN)" = local
go version | grep -q "go1.25.14"
node --input-type=module -e "import(\"/opt/zenchron-browser/playwright/index.mjs\").then(m => { if (!m.chromium) process.exit(1) })"
/opt/zenchron-browser/chrome/chrome --version
# every pinned package is installed at exactly its pinned version
dpkg-query -W -f="\${Package}=\${Version}\n" $(cut -d= -f1 /expected-packages.txt) | sort | diff - /expected-packages.txt
'

if [ -n "$reference" ]; then
  env_of() { confined "$1" -c 'go env GOROOT GOTOOLCHAIN GOPATH GOFLAGS GOPROXY GONOSUMDB GOTOOLDIR; go version; printf "%s\n" "$PATH"'; }
  [ "$(env_of "$image")" = "$(env_of "$reference")" ] || { echo "the Go environment differs from the reference image"; exit 1; }
  echo "Go environment identical to the reference image"
fi
echo "image prerequisites hold"
