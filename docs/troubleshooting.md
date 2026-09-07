# Troubleshooting

Operator remedies for `autonomy doctor` checks that FAIL. Each entry states
what the check proved, why it is a FAIL rather than a warning, and the exact
command that repairs it.

## `assurance.dependency_cache` — the dependency cache is EMPTY

```text
FAIL assurance.dependency_cache  the dependency cache /var/lib/zenchron/modcache is EMPTY.
     Offline verification reads pre-warmed operator-provisioned modules and downloads
     nothing, so an empty cache cannot verify any candidate; provision it from the
     trusted base module graph with the pinned image
```

The directory named by `assurance.dependency_cache_dir` exists but holds no
entries. Provision it with the command below.

### Why verification cannot fill the cache itself

Assurance runs with **no network, by contract**. Every verification container
is started with `--network none`, `--read-only`, `--cap-drop ALL`, and
`GOPROXY=off`, and the dependency cache is bind-mounted **read-only** at
`/cache` for both the preparation step (`go list -deps ./...`) and the verdict
step (`gofmt -l .`, `go vet ./...`, `go test ./...`). Nothing inside that
boundary can download a module or write one into the cache.

That is deliberate, and it is the reason an empty cache is an operator
problem:

- **The cache is trusted material, like the pinned image.** It is the module
  graph a candidate is verified *against*. If a run could populate it, the
  candidate under judgement would be choosing the dependencies used to judge
  it.
- **Preparation used to mount it writable** and run `go mod download` into it.
  That let one candidate's module graph write into dependency state every
  other run reads. The mount is read-only now precisely so that cannot happen
  again.
- **So an empty cache is a false readiness claim, not a warning.** `doctor`
  reports what was *proven*. A cache that exists and holds nothing can verify
  no candidate at all, so PASSing (or WARNing) it would report readiness the
  runtime does not have. It is also never a verdict about a candidate: the
  verifier classifies it as the prerequisite failure
  `dependency_cache_unavailable`, because re-running the identical command
  against the identical environment produces the identical failure.

Provisioning is therefore the one step that is allowed to reach the network,
and an operator has to run it deliberately, outside any run.

### Provision the cache

Fill in the three values from the operator configuration and the trusted base
checkout, then run the command as-is:

```bash
CACHE_DIR=/var/lib/zenchron/modcache            # assurance.dependency_cache_dir
ASSURANCE_IMAGE=sha256:0123...                  # assurance.image, exactly
BASE_CHECKOUT=/srv/zenchron-engineering         # the trusted BASE commit, not a candidate

mkdir -p "$CACHE_DIR"

docker run --rm \
  --mount "type=bind,src=$BASE_CHECKOUT,dst=/base,readonly" \
  --mount "type=bind,src=$CACHE_DIR,dst=/cache" \
  --workdir /base \
  --user "$(id -u):$(id -g)" \
  --env HOME=/tmp \
  --env GOTOOLCHAIN=local \
  --env GOMODCACHE=/cache \
  --env GOCACHE=/tmp/go-build \
  --env GOFLAGS=-mod=mod \
  "$ASSURANCE_IMAGE" \
  go mod download all
```

Why each part is what it is:

- `$ASSURANCE_IMAGE` is the same digest the verifier runs, so the toolchain
  that writes the cache is the toolchain that reads it. It must be the value
  `docker image inspect --format '{{.Id}}' <image>` prints — the runtime
  refuses to start a sandbox whose configured image does not inspect back to
  itself. To pin one for the first time:
  `docker pull golang:1.25 && docker image inspect --format '{{.Id}}' golang:1.25`.
- **No `--network none` here.** This is the only step permitted to talk to the
  module proxy and the checksum database; leave `GOPROXY`/`GOSUMDB` at their
  defaults so what lands in the cache is checksum-verified.
- `dst=/cache` with `GOMODCACHE=/cache` writes the modules exactly where
  verification later mounts and reads them.
- `$BASE_CHECKOUT` is mounted **read-only** and must be the trusted base
  commit — not a candidate worktree. A candidate tree can add a dependency,
  and provisioning from it would write that candidate's chosen module into the
  material every other run is verified against. Read-only also means a base
  tree with an incomplete `go.sum` fails loudly ("read-only file system")
  instead of being silently rewritten.
- `go mod download all` covers the test dependencies of dependencies, which
  `go test ./...` needs and a bare `go mod download` may omit.
- `--user "$(id -u):$(id -g)"` with `HOME=/tmp` leaves the cache owned by the
  operator rather than by root. Verification only ever reads it.

### Confirm it

```bash
go run ./cmd/zenchron-engineering autonomy doctor --text
```

`assurance.dependency_cache` should now PASS and name a `cache/download`
module tree — that subdirectory is what a pre-warmed cache is for.
`assurance.dependency_preparation` should PASS alongside it, stating that the
pinned image runs the Go toolchain offline and the cache is mounted read-only
into every verification.

## `assurance.dependency_cache` — no cache is configured

```text
FAIL assurance.dependency_cache  no assurance.dependency_cache_dir is configured;
     offline verification has no trusted module material to read and never downloads any
```

Name an absolute path in the operator layer — the configuration file outside
every repository the runtime works on, `$ZENCHRON_CONFIG` or
`<user config dir>/zenchron/config.json`. A repository cannot name it:

```json
{
  "assurance": {
    "image": "sha256:0123...",
    "dependency_cache_dir": "/var/lib/zenchron/modcache"
  }
}
```

Then provision it with the command above.

## Assurance fails with `module_unavailable_offline`

The cache is provisioned, but the exact tree needs a module it does not hold —
typically because `go.mod` or `go.sum` changed since the cache was last filled.
The symptom is a prerequisite failure carrying
`the trusted offline cache does not contain a module the exact tree requires`,
not a test failure.

Re-run the same provisioning command against the trusted base checkout at its
current commit. Never point it at the candidate tree to make one run pass:
that hands the candidate authorship of the material used to verify it.

## Assurance fails with `dependency_cache_unavailable` on a non-empty cache

The container could not read the cache — usually ownership or mode on
`$CACHE_DIR` after it was provisioned as a different user, reported as
`permission denied`. The verifier needs the whole tree readable, and nothing
more; it never writes to it. Re-check ownership, or re-run the provisioning
command with the `--user` value that matches how the cache was created.
