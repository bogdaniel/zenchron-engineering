# Concurrency: lanes, worktrees and in-process guarantees

Two kinds of concurrency matter for this module: several agents editing it at
once (§1–§4), and several goroutines using it at once (§5). The first is the
protocol Gate A was built under and the default for future changes; it adds to
the root `AGENTS.md` and issue #446 §4/§13 and never overrides them.

## 1. Gate A build record

| Item | Value |
| --- | --- |
| Base | `27b7c30` (main, merge of #503) |
| Branch / worktree | `claude/446-agent-kernel-gate-a` in `/Users/m3tagh0st/dev/zen-446` (a dedicated worktree; the main checkout was not used) |
| Write allowlist | `agentkernel/**` and `.github/workflows/agent-kernel.yml` only (`scripts/check-scope.sh`) |
| Mode | one shared checkout, disjoint file ownership, coordinator alone runs mutating Git commands |

Commit sequence on the branch: wave 0 (`4664540` ADR, frozen API, boundary
scripts, CI; `13bce23` schemas and examples), wave 1 lane integrations
(`21f07c8` context/memory/routing, `2152543` tools/storage, `1b8c803`
engine/providers, `0f14510` intelligence), coordinator API changes
(`f7f3036` execution-scoped replay and cache-write usage, `4696e31` unknown
cache rates), then wave 2 in the same checkout: `2ef5954` harness (examples,
eval corpus, `cmd/kernel-eval`), `0ac6f55` documentation, `17adf9c`
independent review (architecture and conformance tests, deliberate-break
record) and `1406256` lane repairs of the review findings.

## 2. Ownership

Ownership is by path, from `AGENTS.md`:

| Paths | Owner | Depends on |
| --- | --- | --- |
| `api/`, `schemas/`, `go.mod`, `AGENTS.md`, `README.md`, `scripts/`, `.github/workflows/agent-kernel.yml`, `docs/adr/` | coordinator | — |
| `engine/`, `providers/` | execution lane | `api`, `context`, `routing`, `tools` |
| `tools/`, `storage/` | tools/storage lane | `api` |
| `intelligence/` | intelligence lane | `api`, `storage` |
| `context/`, `memory/`, `routing/` | context/memory/routing lane | `api`, `storage` |
| `cmd/kernel-eval/`, `examples/`, `testdata/eval/`, `docs/benchmarks/` | harness lane | every production package (read-only) |
| `tests/conformance/`, `tests/architecture/`, `docs/acceptance/` | independent review lane | everything (read-only) |
| `README.md` (drafted), `docs/architecture.md`, `docs/spec/`, `docs/concurrency.md`, `docs/integration-plan.md` | documentation lane | everything (read-only) |

A lane writes only its paths. Reading anything is allowed. Lane roles are
temporary build assignments, not runtime or domain agents.

## 3. Protocol

1. **Freeze before fan-out.** `api/` and `schemas/` are frozen before
   behavioural lanes start. A lane that needs a contract change sends the
   coordinator the acceptance case that requires it; it never edits a shared
   DTO itself. The coordinator lands the change (schema and examples in the
   same commit, version bump if incompatible) and lanes rebase their work onto
   it.
2. **Git is the coordinator's.** In a shared checkout, lanes run no mutating
   Git command (`add`, `commit`, `stash`, `checkout`, `reset`, `rebase`,
   `clean`). The coordinator stages explicit paths only (never `git add -A`)
   and reviews staged and final diffs.
3. **Disjointness is checked, not assumed.** Before integrating, run
   `bash scripts/check-scope.sh` (merge-base against `BASE_REF`, both sides of
   renames, deletions, untracked files, symlink targets) and
   `git status --porcelain` to confirm every changed path belongs to the lane
   being integrated.
4. **Overlap stops one item, not the build.** If two lanes need the same file,
   stop that work item, report the overlap to the coordinator, and continue
   independent work.
5. **No parent edits.** Nothing outside the allowlist is written — no root
   config, `go.work`, root `go.mod`, existing workflow, or existing docs. A
   failing root test is reported with evidence to its owning issue, not fixed
   here.
6. **Offline only.** No lane spends provider credit or uses live credentials;
   scripted and fake transports only.
7. **Docs last, against final code.** The documentation lane re-reads the code
   after the other lanes land and states any divergence instead of papering
   over it.
8. **Handback.** Each lane returns changed paths, tests run with results,
   negative-test evidence, assumptions, blockers and documentation impact.

## 4. Future changes

- Prefer a separate worktree and branch per change (`git worktree add`); use
  the shared-checkout mode above only when tooling forces it.
- The #446 scope checker applies to this feature lane. Unrelated repository
  PRs are not bound by it; the module workflow runs only when `agentkernel/**`
  or its workflow changes.
- Broadening `scripts/check-scope.sh`, `scripts/check-imports.sh` or
  `tests/architecture` from the change they check requires independent review.
- Gate B work is a separate issue with its own allowlist
  (`docs/integration-plan.md`); it does not reuse this lane's permission.

## 5. In-process concurrency guarantees

| Component | Guarantee | Mechanism |
| --- | --- | --- |
| `engine.Engine` | safe for concurrent `Execute` calls; executions share no mutable state | per-call `run`; config is read-only |
| `engine.ledger` | concurrent reservers cannot overspend through independent prechecks | one mutex; all-or-nothing multi-dimension reserve (`TestConcurrentReservationsCannotOverspend`) |
| one execution | the loop runs on one goroutine; tool-side writes (events, artifacts, account) are serialized | `run.mu` |
| `storage.Memory*`, `storage.File*` | concurrent readers and writers | mutexes; atomic file replace (`TestRecordsConcurrentReadersWriters`, `TestArtifactsConcurrentPutGet`) |
| `memory.Store` | serialized read-settle-write per store | `sync.RWMutex`; one store per `storage.Records` (`TestConcurrentReadersAndWriters`) |
| `intelligence.Index` | immutable after build; overlays private | no shared mutable state (`TestConcurrentOverlaysAreIsolated`) |
| `providers/scripted` | concurrent callers each consume one step | mutex (`TestConcurrentCallsConsumeEachStepOnce`) |

The ledger is a per-execution resource limiter. It is not a scheduler, queue
or cross-execution capacity manager; global provider/account capacity is the
host's. `go test -race ./...` runs in the module workflow.
