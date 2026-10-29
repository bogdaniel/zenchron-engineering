# The browser-capable assurance image (docs/assurance-image.md).
#
# Everything that varies is pinned: both base images by immutable digest, every
# Debian package by exact version from one dated snapshot, the Playwright driver
# by package-lock.json. A change to any of them is a change to a file in this
# directory, so a different image is always a reviewed diff.
ARG GO_IMAGE=golang@sha256:699337d620559a59b4a2bb298ad59611e535d2ee755a34cf2d2a98f37578dc80
ARG NODE_IMAGE=node:24-trixie-slim@sha256:43b9ba1142b8dcf33fa8f922ca1be85714adc444e0586c7cf7726daaeabc1bea

FROM ${NODE_IMAGE} AS node

FROM ${GO_IMAGE}
# The Go toolchain, GOTOOLCHAIN=local and the environment of the base are left
# exactly as the base has them; this image only adds to it.

# Chromium and its whole dependency closure, each at the exact version resolved
# against the snapshot in debian-snapshot.sources. apt refuses to install a
# version the snapshot does not hold, so a drifting archive fails the build
# instead of changing the image.
COPY debian-snapshot.sources /etc/apt/sources.list.d/pinned.sources
COPY chromium-packages.txt /tmp/chromium-packages.txt
RUN find /etc/apt/sources.list.d -type f ! -name pinned.sources -delete \
 && rm -f /etc/apt/sources.list \
 && echo 'Acquire::Check-Valid-Until "false";' > /etc/apt/apt.conf.d/99snapshot \
 && apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends $(cat /tmp/chromium-packages.txt) \
 && rm -rf /var/lib/apt/lists/* /tmp/chromium-packages.txt

# Node 22 or later: the browser scripts use the global WebSocket.
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
 && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx

# The exact paths the runtime requires (runtime/browser_acceptance.go).
WORKDIR /opt/zenchron-browser/playwright
COPY package.json package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund \
 && ln -s node_modules/playwright-core/index.mjs index.mjs \
 && mkdir -p /opt/zenchron-browser/chrome \
 && ln -s /usr/bin/chromium /opt/zenchron-browser/chrome/chrome
WORKDIR /
