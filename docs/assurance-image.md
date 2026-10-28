# The browser-capable assurance image

`docs/browser-acceptance.md` says what the browser check needs from
`assurance.image`: Node 22 or later, Playwright and Chrome at fixed paths, and a
few GNU utilities. This document is how that image is built, checked, provisioned
and rolled back. The files are in `assurance-image/`. Building or pinning the image
changes nothing about how the verifier runs: it is the same image slot, with the
same restrictions (no network, read-only root, all capabilities dropped,
`no-new-privileges`, default seccomp).

## What is pinned

| Input | Pin |
|---|---|
| Go toolchain and base OS | `GO_IMAGE` in the `Dockerfile`, by `sha256` digest: the image in service when this was written (Go 1.25.14, Debian 13) |
| Node | `NODE_IMAGE`, by `sha256` digest. Only `node` and `npm` are taken from it; the version is whatever that digest holds and is recorded at build time |
| Chromium, `fonts-liberation` and their whole dependency closure | `chromium-packages.txt`: every package at an exact version, resolved against the dated archive in `debian-snapshot.sources` (main and security, same timestamp) |
| Playwright driver | `package.json` (exact version) and `package-lock.json` (registry URL and integrity hash), installed with `npm ci --ignore-scripts` |

`runtime/assurance_image_test.go` fails the build of the repository if any of these
floats: a tag instead of a digest, a wildcard or range, two snapshot dates, a
lockfile entry without an integrity hash, or a path the runtime requires that the
build does not create.

The Go toolchain, `GOTOOLCHAIN=local` and the Go environment of the base are not
modified; the image only adds to it. `verify.sh` compares the Go environment with a
reference image and fails on any difference.

## Building

```sh
assurance-image/build.sh zenchron-assurance:browser-<date> record.json
assurance-image/verify.sh zenchron-assurance:browser-<date> <image in service today>
```

`build.sh` refuses a tag that already exists, never removes or prunes anything, and
never touches the configured `assurance.image`. `record.json` holds the immutable
image ID, the architecture and the versions of Go, Node, npm, Chromium and
Playwright, and the hashes of the five input files.

Changing the Chromium set is deliberate and reviewable: change the timestamp in
`debian-snapshot.sources`, run, from the repository root,
`assurance-image/resolve-packages.sh > resolved.txt && mv resolved.txt assurance-image/chromium-packages.txt`,
review the diff, rebuild. A failed apt simulation exits non-zero and prints
nothing, so the `mv` does not run and the committed list is never replaced by a
partial one. `resolve-packages.sh` is deterministic for a given snapshot:
regenerating it reproduces the committed list.

## What "repeatable" means here

The inputs are pinned, so a rebuild installs the same packages, the same Node and
the same Playwright files. Two builds of the same inputs, one of them with
`--no-cache`, produced identical package versions and identical hashes of `node`,
the Chromium binary, the Playwright tree and the npm tree. The **image ID still
differs between builds**: layer metadata carries build times. So the pin that
matters is the ID of the specific image the operator built and validated, which
`record.json` captures; it is not a property of the sources alone. Bit-for-bit
reproducibility would need timestamp rewriting, which is not claimed.

The Debian snapshot is also an external service. A build fails closed if it cannot
serve an exact pinned version, but it cannot be rebuilt if the service is
unavailable.

## Security considerations

- Chromium, Node and npm are in the image for **every** verification, not only the
  browser step, because the baseline verifier uses one image for both. They run with
  no network and the same restrictions as the Go suite.
- Chrome cannot start its own sandbox under those restrictions. The browser tests
  launch Chromium with `--no-sandbox`; the container is the boundary
  (`docs/browser-acceptance.md`). Nothing here widens that boundary.
- `npm ci --ignore-scripts` runs no package install scripts at build time.
- Build time needs network (registry, Debian snapshot, Docker Hub). Verification
  time needs none.
- The driver version and the Chromium version are not released together. They worked
  together in every recorded run; a change to either is a change to be validated.

## Provisioning, retention and rollback

1. Build and verify as above. Record the image ID.
2. Run the strict real-container test on the image, repeatedly
   (`ZENCHRON_BROWSER_IMAGE=<id> ZENCHRON_MODCACHE=<modcache> go test ./runtime -run TestBrowserAcceptanceInARealContainer -count=1`).
3. Only then set `assurance.image` to the image ID. That is a configuration change
   and a cold re-adoption, authorized separately.
4. Keep the previous image (the Go-only one) and every image built here until the
   new one has served real verifications. Do not run `docker system prune` before
   that.
5. Rollback is setting `assurance.image` back to the previous ID and re-adopting. A
   record produced under the browser image names that image, so it is never
   mistaken for evidence from the old one.
