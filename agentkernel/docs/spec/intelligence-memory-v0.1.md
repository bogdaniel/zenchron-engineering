# Repository intelligence and memory v0.1

Normative for Gate A. Code: `intelligence/*`, `memory/*`, `storage/records.go`.
Neither package carries engineering meaning: no ownership, impact, security,
policy or assurance interpretation (that is #67), and no learned strategy
(#70). Every fact is an observation of workspace bytes; every remembered record
is untrusted on retrieval.

## Part I — Repository intelligence

### 1. Snapshot identity

`intelligence.Identity` is everything a snapshot's facts depend on;
`Identity.Key()` is `api.Digest` of its canonical JSON (map keys sorted, slices
canonicalized). Equal keys are interchangeable; any difference is a miss.

| Component | Source |
| --- | --- |
| `schema` | `SchemaVersion` = `agentkernel.intelligence/v1` (persisted shape) |
| `extractor` | `ExtractorVersion` = `go-structural/1`; bumped when a rule change would alter facts from identical input |
| `extractor_runtime` | `runtime.Version()` of the process whose `go/parser`/`go/types` ran |
| `manifest_digest` | `Manifest.Digest()` over the selected files (§2) |
| `settings` | `Settings{go_version, goos, goarch, build_tags, cgo_enabled}`, always caller-supplied, never read from the host environment; tags sorted and de-duplicated |
| `go_mods` | each manifest `go.mod` path → content digest |
| `scope` | `Scope{include, exclude, packages}`, sorted sets |

`go_version` must look like `go1.N[.P|rcN|betaN]`; it selects the `go1.1 …
go1.N` release tags build constraints see.

### 2. Manifest and scope

`Manifest{files: [{path, digest, size}], skipped}` is deterministic (sorted).
Only regular files are hashed; symlinks and other non-regular entries are
listed in `skipped` and never followed, so no manifest reads outside its root.
`.git` is always excluded. `Scope.selects` is the single selection rule for the
full walk, the bounded walk and overlays: `include`/`exclude` are `path.Match`
patterns tested against the path, each parent directory and the base name
(`exclude` wins); `packages` bounds extraction to those directories
(non-recursive) plus every `go.mod` on their ancestor chain.

**No mandatory full indexing.** With `Scope.Packages` set, `Build`/`Open` read
only those directories and their `go.mod` chain. `Stats` reports files hashed,
bytes hashed, files read and parsed, packages checked, directories extracted
and reused, manifest and extraction time, cache hit and degradation.

### 3. Derivation

1. `go.mod` files are parsed for module path and `go` directive; an unparsable
   one is recorded incomplete (`bad_go_mod`); a package directory with no
   governing module gets a placeholder import path (`no_module`).
2. Build constraints are evaluated with a `go/build.Context` built from
   `Settings`, whose `OpenFile` reads only manifest-verified bytes.
3. Files are parsed with `go/parser` and type-checked with `go/types` over
   **indexed packages only**: every other import resolves to an empty package,
   so selectors into it become explicit unresolved relations, never GOROOT or
   network lookups.
4. Every byte read after the manifest is re-hashed; a mismatch stops
   extraction with `ErrStaleWorkspace` rather than analysing content the
   identity does not describe.

Facts per package directory (`DirFacts`): files (with `included`/`reason`),
packages (an external `_test` package is its own `Package` with `test`),
symbols, imports (status `indexed`, `module_not_indexed`, `stdlib_not_indexed`,
`external_not_indexed`, `cgo`), references (one per from/to), calls, and
incomplete records. Every relation carries `Evidence{file, line, method,
confidence}`:

| Method | Meaning | Confidence |
| --- | --- | --- |
| `ast-syntax` | read from the parsed file | deterministic |
| `go-types` | resolved by `go/types` over indexed packages | deterministic |
| `heuristic-name` | `TestXxx` ↔ `Xxx` by name | inferred |
| `test-static-call` | a test statically calls the symbol | inferred |

**Calls** are classed `static` (go/types-resolved direct call into an indexed
package), `possible` (interface or type-parameter method; callee is the
abstract method), `dynamic` (function value, field, computed expression,
dereferenced function pointer, reflection) or `unresolved` (callee not
resolvable, usually unindexed). No complete call graph is claimed.

**Test associations** are always `inferred` and carry `Limit =
CoverageUnknown`: suggested tests never replace mandatory assurance.

**Incomplete analysis** (`parse_error`, `build_excluded`, `cgo`, `type_errors`,
`no_module`, `bad_go_mod`, `import_cycle`, `package_name_mismatch`) is recorded,
never dropped. A missing edge is never proof of no dependency.

### 4. Queries

`Index.Callers`, `Index.Callees` return `CallResult{calls, complete, limits}`;
`complete` is true only when nothing in the snapshot could hide a further edge
(any non-static relation, an unknown symbol, an exported symbol whose external
callers are invisible, a bounded scope or any incomplete record makes it
false). `Index.SuggestedTests` returns `TestResult` with `complete` always
false; an empty answer adds "not evidence that no test exercises the symbol".
`Index.Snapshot()` returns a deep copy; mutating it cannot affect the index.

### 5. Overlays (execution-scoped)

`(*Index).Overlay(ctx, OverlaySpec{Root, Changes, Settings})` derives a new
index; the receiver is never modified and the result is private to its caller.

- `Change{path, content, delete, renamed_from}`: content is the complete new
  file. Paths must be clean workspace-relative file paths; a path may change
  once; a delete carries no content or rename; deleting or renaming a file the
  base manifest lacks is refused. Changes outside the snapshot scope are listed
  in `OverlayBinding.ignored`; a rename into scope from outside is an addition.
- Unchanged files are read from `Root` only when a re-extracted package needs
  them, and must match the base manifest digest.
- `OverlayBinding{base_key, dirty (path → digest), deleted, renamed (new →
  old), ignored, settings, invalidation}` binds the overlay to its base.

Invalidation (`invalidation` field):

| Value | Trigger | Re-extracted |
| --- | --- | --- |
| `files` | `.go` edits, additions, deletions, renames | changed directories and every directory that transitively imports them under the old or new import path |
| `module` | a `go.mod` change alters a directory's governing module | those directories, plus importers |
| `all` | `Settings` differ from the base (tags, toolchain, GOOS/GOARCH, cgo) | everything |

Deletion and rename remove the old file's relations because the affected
directory is re-extracted from the overlay manifest. Two overlays of one base
share only the base's read-only facts and (for an in-memory base) its
read-only `go/types` packages; an index reloaded from cache has none, so its
overlays re-check unaffected dependencies from verified bytes.

### 6. Views (`intelligence.View`, an `api.ContextSource`)

`NewView(ix, workspace)` refuses unless `workspace.ManifestDigest` equals the
snapshot's manifest digest (`ErrSnapshotMismatch`). The host proves the view
describes the request's bytes by using the intelligence manifest digest (or an
equivalent computed with the same selection) as `WorkspaceRef.ManifestDigest`;
the kernel does not compute `WorkspaceRef` itself. `ContextItems` refuses a
query whose workspace id or digest differs from the view's. Items are optional,
`trust: workspace`, `revision` = index key, ids `intel.<kind>.<16 hex>`, kinds
`dependency` (symbol with callers/callees and their completeness), `test`
(suggested tests with limits) and `source` (file declarations). Relevance is a
deterministic lexical score against the objective; ordering is score then id.

### 7. Cache and storage

`Save(ctx, records, ix)` stores a base snapshot in `storage.Records`
partition `intelligence.index` under its identity key as
`{format: 1, checksum, snapshot}`; overlays are refused. `Load` verifies
format, checksum, strict decoding, identity key and non-overlay, else wraps
`ErrCacheInvalid`. `Open(ctx, records, cfg)` always hashes the current
workspace first, so a cache can save extraction work but never serve facts for
other bytes; any read or write failure falls back to a fresh build and is
reported in `Stats.CacheDegraded`; `records == nil` disables caching ("cache
disabled"). Cache failure affects efficiency, never permission or correctness.

## Part II — Memory

### 8. Records

`memory.Record` (`RecordVersion` = `agentkernel.memory/v0.1`): `id`, `kind`
(`observation` | `inference` | `summary`), `partition`, optional `subject`,
`content`, `source_digests` (≥1, sha256), `derivation{method, tool?, model?,
version}`, `created_at`, `validated_at`, `depends_on`, `validity` (`valid` |
`stale` | `conflicted` | `unknown`), `confidence` (nil = unknown, else in
[0,1]). An `inference` must name its model. There is deliberately no
authoritative field and no promotion path. `Store.Put` defaults empty
`version`, `validity` (`valid`) and `created_at` (now), then validates.

### 9. Partitions

`Partition{repository, workspace, access}`, each a valid identifier. Storage
partition = `memory-` + sha256 of its JSON, so components cannot alias. Reads
require the exact partition; there is no cross-partition listing. A record
stored under another id or partition decodes as corrupt.

### 10. Validity, invalidation and conflicts

`settle` re-derives validity on every read and before every write, so a crash
between writes cannot surface a contradiction or stale basis as valid:

- valid or conflicted records sharing a `subject` with differing content all
  become `conflicted` and stay so until `Store.Resolve(p, subject, winner)`
  (winner and identical-content records → `valid` with `validated_at`; the rest
  → `stale`);
- a valid record whose dependency is missing or not valid becomes `stale`,
  transitively.

`InvalidateSource(p, digest)` and `InvalidateDerivation(p, derivation)` (exact
equality) mark matching records and their dependents stale. Validity never
expires by time; `MaxAge` is retention (§11), not validity.

### 11. Retention and bounds

`memory.New(records, Limits{MaxRecords, MaxBytes, MaxAge}, now)`; records and
bytes are required positive. `Store.Pin(p, id)` returns a release func; pinned
records and everything they depend on are never evicted. Pins are held in
process memory and do not survive a restart. Eviction (`planEvictions`):
unprotected records older than `MaxAge`, then unprotected records — non-valid
first, then oldest, then id — until the bounds hold. A `Put` that cannot fit
without evicting protected records is refused with `ErrFull` before any write;
`Prune` applies bounds without a write.

### 12. Retrieval as context

`Store.Source(p)` is an `api.ContextSource` bound to one partition. It returns
only `valid` records as optional items with `kind: memory`, `trust: memory`,
id `mem:<record id>` (so they cannot collide with request ids), a header naming
kind, derivation and confidence, and `revision` = digest of the sorted source
digests; ordered by confidence then id; `Limit` applies. Stale, conflicted and
unknown records are excluded entirely. The engine additionally demotes any
sourced `host` trust to `memory` and drops items whose id collides with a
request item, so remembered text can never become a system instruction
(`TestRememberedInstructionsStayUntrusted`, `TestTrustBoundary`). The source
ignores `ContextQuery.Workspace`: choosing the partition is the host's binding.

### 13. Corruption and degradation

A record failing integrity (`storage.ErrCorrupt`) or strict decoding is
skipped and named; `Source` then adds a `memory-degraded` item listing the
skipped keys, so degradation is visible in the context manifest.
`Store.DropCorrupt` deletes them for re-derivation. Any other storage error is
returned, never read as an empty partition.

### 14. Atomicity and concurrency

`storage.FileRecords` writes each record atomically (frame
`zkrec1 <sha256> <len>\n` + value, temp file, fsync, rename, directory fsync);
a torn or altered file reads as `ErrCorrupt`. File names are base32hex of the
identifier (case- and `:`-safe). A memory update writes changed records with the
fresh record last, then deletes evictions; it is not a multi-record
transaction, and `settle` keeps any partial state safe to read. One
`memory.Store` per `storage.Records`: its lock serializes read-settle-write;
two stores over one backing store are unsupported.

## Part III — Storage partitions and unknowns

| Partition | Owner | Key |
| --- | --- | --- |
| `memory-<sha256>` | `memory.Store` | record id |
| `intelligence.index` | `intelligence.Save/Load/Open` | `Identity.Key()` (`sha256:<hex>`) |

`storage.MaxRecordBytes` = 16 MiB per value. Roots are explicit absolute
paths; nothing assumes the parent checkout. No raw database handle is exposed.

Known unknowns and limits:

- The `intelligence.index` partition has no retention or eviction; snapshots
  accumulate per identity until the host deletes them.
- Intelligence covers Go plus a generic file manifest only.
- Relevance scoring is lexical; it is not a quality claim.
- Pins (memory) are process-local; a restarted process protects nothing until
  work re-pins.
