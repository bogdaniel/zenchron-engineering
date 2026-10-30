# CI evidence tiers

Issue #516. Merging code is an integration event; trusting it for adoption is
an authorization decision. CI evidence is split into tiers so that ordinary
pull requests do not pay for the whole repository suite, without any evidence
disappearing.

| Tier | What | When | Where |
|------|------|------|-------|
| T0 | `gofmt`, `go vet`, Windows build and vet | every PR | `ci/static.sh` |
| T1 | every package except `runtime` whole, plus the `runtime` tests the impact registry selects, run as `evidence-static`, `evidence-packages` and four `evidence-runtime` shards that the required `evidence` check aggregates | every PR | `go run ./ci/evidence` (all of it), `-packages`, `-shard 1/4`–`4/4` |
| T1 race | the same selected high-risk (`race: true`) test names partitioned across eight isolated runners by measured `-race` cost; every part must pass | every PR | `go run ./ci/evidence -race -race-shard 1/8` (and 2/8–8/8) |
| T2 | `go test -json -timeout 45m ./...` | every push to `main` only (the `go` job); not run on pull requests | `.github/workflows/ci.yml` |
| T3 | `go test -race ./...` | nightly, on request | `.github/workflows/assurance.yml` |

## Status

**Stage 7 is active** (#516, [ADR-0007](adr/0007-trusted-main-from-revision-evidence.md)).
A pull request is gated by T0, T1 and the risk-selected race shards: the
`trusted-main-adoption` ruleset requires `evidence` and `evidence-race`, strictly,
from GitHub Actions. The full suite runs once per merge, as the `go` job on the
push to `main`, and is not a pull-request check. Stages 0–3 and the T3 sweep are
in place.

Merging and trusting are separate. A green pull request is eligibility to merge.
A revision becomes `trusted_main` only with an accepted, exact-SHA, push-event T2
(`go`) result for that commit, and controller adoption follows `trusted_main`. A
red or pending T2 holds trust at the previous accepted revision and never blocks
the next independent pull request.

The ruleset no longer carries the trust proof. ADR-0007 separated `main_head` from
`trusted_main`, so `VerifyTrustRoot` (`runtime/trust_root.go`) requires no status
check and renaming or replacing a pull-request job cannot break adoption; the
exact-revision resolver (`runtime/trusted_revision.go`) accepts one producer, the
`go` job from `ci.yml` on a push to `main`. That identity, its command and its
trigger are held by `ci/evidence/workflow_test.go`.

The evidence behind the promotion is [ci-stage7-promotion-audit.md](ci-stage7-promotion-audit.md).
The ordered, reversible activation and rollback procedure is
[ci-stage7-migration.md](ci-stage7-migration.md).

## Baseline (stage 0)

Measured from CI runs of the `go` job on 2026-10-06 and 2026-10-07:

- the whole job takes 13–18 minutes per run, on every PR and on every push to
  `main`;
- the `Test` step takes about 12 minutes of that, and the static steps about
  30 seconds;
- `runtime` accounts for 655 s of the test time, and every other package takes
  under 7 s (`cmd/zenchron-engineering` 7 s, the rest under 1.1 s);
- strict required checks mean a PR whose base moves runs the whole job again.

A local `go test -json ./runtime` (1302 s, about twice CI's time) gives the
serial cost of each domain. Locally, `scheduler` takes 180 s, `orchestration`
154 s, `planning` 144 s, `handoff` 133 s, `supervisor` 126 s and `workgraph`
113 s. Every other domain takes under 110 s. Under `-race`, the `handoff`
domain takes 507 s instead of 127 s, which is why the race escalation runs as
its own job.

The 30 most recent merged PRs (#461 to #527), replayed through the selector.
CI times are estimated as half the local times; race is estimated at four times
the plain run:

- 9 PRs were docs-only, CI-only, or outside `runtime`, and selected only the
  guards;
- 10 narrow and medium `runtime` PRs selected 75–850 tests, about 0.5–5 minutes
  of CI time for the plain T1 run;
- 11 PRs touched `runtime/operations.go` (or an unclassified dependency such as
  the new `execution/` package) and ran the whole `runtime` package, about 11
  minutes;
- the race escalation is the long pole: about 2–6 minutes for narrow PRs, and
  10–20 minutes for wide ones.

So the 2–5 minute target holds today for narrow PRs that avoid
`operations.go`. Two things stand between that and most PRs:

- **Splitting `operations.go` (#495).** It is the biggest latency lever. Until
  it lands, more than a third of runtime PRs will run the whole package, and
  that cost is honest, not overhead.
- **How race escalation gates merging.** That is a stage-7 decision for
  ADR-0007.

## The impact registry

`ci/evidence/registry.json` is the one mapping from changed files to evidence.
The workflow has no path list of its own. Each domain names the source globs
whose change selects it, and the test-file globs (or test names) that make up
its evidence. `race: true` marks a high-risk domain, which also runs its tests
under `-race`. `always: true` marks the source-scan guards. `whole` lists
cross-domain hotspots. `runtime/operations.go` spans review, provider and toolchain
semantics. `schemas/validate.go` registers and compiles every kernel/planning/API
contract; changes there retain whole-runtime and every high-risk race domain.

Selection is deterministic. Each changed path is handled by the first rule that
matches:

1. **Inert** paths select no focused-runtime evidence. T1 still runs every
   other Go package whole. Control-plane templates are covered by those package
   tests; browser assets are covered by the isolated browser acceptance when
   available. The product and WorkGraph schemas are compiled and validated by
   the whole `schemas` package run.
2. A test file outside `runtime` is covered, because its package runs whole.
3. A **hotspot** runs the whole `runtime` package.
4. A `runtime` test file with no tests of its own is a shared helper, so it
   fails safe to the whole package.
5. A changed or deleted `runtime` test file selects every domain whose `tests`
   it matches. Those domains then run in full, including their race policy.
   A source path selects every domain whose `sources` it matches.
6. Any other path in `runtime` (including a test file no domain owns), or in a
   package `runtime` imports, **fails safe** to the whole `runtime` package.
7. Any other path inside a package is covered by that package's whole run.
8. Anything else, such as `go.mod`, fails safe to the whole package.

Whenever the plan falls back to the whole package (rules 3, 4, 6 and 8), the
race job escalates **every** `race: true` domain. Broad or unknown impact must
never carry less race evidence than a narrow, known one. That is a large run, an
estimated 15–20 minutes of CI, but it still stops short of the full
`go test -race ./runtime`, which stays the job of T3.

The job prints why each domain was selected:

```text
T1 evidence for 3 changed path(s) since origin/main:
  guards, 6 tests: always runs
  handoff (race), 48 tests: runtime/handoff_repair.go changed
./runtime: whole=false, 54 selected test(s), 48 under -race
```

Run `go run ./ci/evidence -plan` to see the plan for your branch without running
it.

### Keeping it honest

The conformance tests in `ci/evidence` run in T1 on every PR. They fail when:

- a `runtime` source file is not classified by any domain or hotspot;
- a `runtime` test file is not reachable from any domain;
- a pattern matches no file, a named test does not exist, or a domain selects
  no tests.

So when a PR adds a file to `runtime`, the same PR has to classify it. Deleting
a rule leaves its files unclassified, and the conformance tests catch that.

A change in one domain can still break a test in another, through shared code.
T2 exists to catch that case. How often it happens is the measure of whether
the domains are drawn well.

### Engineering OS classification

`runtime/operator_action_receipt.go` and its test belong to `store` (`race: true`):
they add closed observational journal events and exact read-only receipt lookup.
Changing either selects the receipt test and the entire durable-store domain,
including payload validation, journal integrity and read/reopen evidence.

`operator_surface` explicitly owns active `frontend/**`, embedded
`controlplane/assets/design/**` and Control Plane wire-schema paths. They are not
declared inert. Its race evidence proves the bounded read contracts, original
receipt, controller proof/role and lost-control-response distinction those
clients consume. Every non-runtime Go package still runs whole, including the
Control Plane, schema compiler and real controller integration tests.

This is a **runtime boundary domain**, not browser acceptance. Strict TypeScript,
reproducible bundling, actual browser/keyboard/contrast/reflow checks and human
screen-reader/operator qualification remain explicit obligations in the
[operator guide](design/operator-acceptance.md). Opt-in browser tests are not
silently represented as having run on a Go-only CI host.

The current PR also changes the shared schema registry, so it still selects the
whole runtime and all high-risk race domains. This classification does not remove
that cost or establish full race acceptance. Unknown/new dependency paths still
fail safe to the whole runtime; no timeout, gate or guard is weakened.

### Race workload partitioning

The reviewed head exhausted its existing 60-minute package bound after selecting
1285 race tests; the active test had run only three seconds. Exact trusted main
also shows substantial serial cost (80.6 minutes for its successful full T3).
T1 race partitions the **same sorted selected names** across eight
isolated runners (it was four, dealt by position; see "Shard balance"). Overlapping domains do not duplicate tests. Conformance tests
prove the partition union equals the entire narrow or whole-fallback selection,
with every name occurring exactly once. No selection or test assertion is cut.

`-race-shard part/count` is valid only with `-race`; malformed/out-of-range values
refuse. The original unsharded CLI remains available. Every part keeps `-race`
and the unchanged 60-minute package bound. Matrix fail-fast is off so a failing
part does not cancel the others. The existing `evidence-race` check identity is a
fail-closed aggregate: failure, cancellation or skip of a part cannot pass it.
This follows GitHub's [needs result](https://docs.github.com/en/actions/reference/workflows-and-actions/contexts#needs-context)
and [matrix failure handling](https://docs.github.com/en/actions/how-tos/write-workflows/choose-what-workflows-do/run-job-variations#handling-failures)
contracts. The strict required `go` job, exact push-to-main T2, trust policy and
unpartitioned whole-module T3 remain unchanged. CI still must prove every part
at the exact submitted head; a focused probe is never that qualification.

### Shard balance

Plain runtime and race selections are both split by name across isolated runners
(`-shard part/count`, `-race-shard part/count`). A shard is only a set of names:
the shards of a selection are a partition of it, and every runner derives the same
partition from the same checkout. Balance is a speed concern and nothing else; a
wrong, missing or stale cost can make a shard slow, and can never add, drop or
duplicate a test (`TestMissingAndStaleMeasurementsNeverChangeWhichTestsRun`).

The names are dealt heaviest first onto the lightest shard, by the cost in
`ci/evidence/costs.json`. A `-race` test is not a constant multiple of its plain
cost (measured ratios run from 1x to 19x), so the file holds two profiles, `plain`
and `race`, each with its provenance (exact commit, hosted runs, configuration,
observation date, number of samples and how they were aggregated). A test the
profile does not list costs the profile's `default`.

To refresh a profile, collect the job logs of hosted runs that ran the shards with
`go test -json`, one directory per run, and run
`python3 scripts/evidence_costs.py 1.0 DIR...`. Per-test cost is the median of the
top-level test's `Elapsed` across the runs, so a parent test is never added to its
own subtests and package setup or runner overhead is not charged to any test. A
test that failed in a run is reported and its duration is not used. The raw
per-run observation of every listed test is kept in `costs.observations.json`, so
a consistently slow test can be told from one spike.
Refresh when the slowest shard drifts well away from the mean.

## #516 acceptance: evidence and the accepted strict-invalidation policy

Measured 2026-10-30 on hosted runners, after Stage 7 and the sharded gate.

| Criterion | State | Evidence |
|---|---|---|
| No `go test ./...` as a PR gate | satisfied | `go` is push-only (`workflow_test.go`); required contexts are `evidence` and `evidence-race` (`conformance_test.go`) |
| Ordinary narrow PRs inside 2-5 min | satisfied | seven hosted gates of 66-171 s, 3-4 s from creation to first job: inert 66-73 s, test-only 102 s, one runtime file 118 s, store and operator-surface tests 165-171 s |
| CI explains what was selected | satisfied | the job log prints the plan with a reason per domain (`-plan` prints it alone) |
| Unknown changes fail safe; high-risk escalates | satisfied | `plan_test.go` (`TestUnclassifiedChangesFailSafeToTheWholePackage`, `TestAWholeFallbackEscalatesEveryHighRiskDomain`) |
| A broken invariant triggers its mapped T1 test | demonstrated | breaking the repairability guard in `runtime/handoff_repair.go` selected the `handoff` domain (67 tests) and failed `TestAnUnrepairableRefusalNeverReachesAModel` and `TestProviderSuccessWithoutAValidHandoffIsNotCompletion`; a `runtime/controller_succession.go` change selects `controller` with 219 race tests |
| Removing a registry rule is caught | satisfied | `TestEveryFocusedSourceIsClassified`, `TestNoRegistryRuleIsDead`, `TestDataFilesOnlyVerificationReadsSelectVerificationNotTheWholePackage` |
| Green T2 advances `trusted_main`; failed T2 does not; the previous one stays identifiable | satisfied | `runtime/trusted_revision_test.go`; observed live on every merge of this series (a failing T2 is covered by fault injection, never induced on `main`) |
| Evidence is bound to an exact subject | satisfied | `TestT2EvidenceIsExactSubjectAndPinnedProducer`; a check is bound to its head SHA |
| `main_head` and `trusted_main` are distinct | satisfied | `TestAdoptionStandsOnTrustedMainNotMainHead` |
| Windows and non-Unix coverage stays in T0 | satisfied | `TestStaticEvidenceKeepsCompilingAndVettingForANonUnixTarget` |
| No second trust database | satisfied | ADR-0007 section 1: `VerifyTrustRoot` requires no status check |
| T2 and T3 still exist | satisfied | `go` on every push to `main`; `assurance.yml` nightly |
| Unrelated PRs merge without repeatedly invalidating each other's evidence | **not met in the strong sense; conservative policy accepted by the owner** | see below |

### Strict up-to-date, and why a base move still reruns

The ruleset requires strict status checks (`strict_required_status_checks_policy`,
pinned by `TestTheRulesetInvalidatesPullRequestEvidenceWhenMainMoves`). A pull
request can merge only if its head contains the current `main`, so any other
merge forces a branch update, a new head and a new `evidence` and `evidence-race`
run. This is ADR-0007 section 6's explicit, conservative invalidation: evidence
for an older base never authorizes a newer head.

Measured over the last 60 merged PRs: 34 of them (57%) needed at least one
update, 55 updates in all (0.9 per PR, at most 3). Classifying what `main` moved
in those 55: 4 touched only inert paths, 4 touched only runtime domains disjoint
from the pull request's own, 42 touched a Go package outside `runtime` (T1 runs
every one of those whole, so the pull request's evidence depends on it), and 5
overlapped or fell back. So domain-aware reuse as section 6 describes it could
have skipped about 15% of updates. A rerun costs a narrow gate (66-171 s); the
cost that remains is the update-and-wait step itself.

The criterion's strong form (unrelated pull requests do not invalidate each
other's evidence) is **not met**, and this document does not report it as met.
Domain-aware evidence reuse is **not implemented**. The owner decided to keep
strict up-to-date as an explicitly accepted conservative policy. The cost is the
measured rerun above: a base move forces an update and a new narrow gate.

Rejected for now: relaxing the ruleset, which ADR-0007 reserves for after
domain-aware reuse exists, and a custom merge queue. GitHub's merge queue is not
available for this personal-account-owned repository; if the repository moves to
an organization, revisit it, since it would make the required checks run on the
exact merge-group commit and needs a ruleset rule and a `merge_group` trigger.

## Developing against these tiers

- Run the focused tests for the behaviour you changed, `ci/static.sh`'s checks
  (`gofmt -l .`, `go vet ./...`, the Windows build and vet), and
  `go run ./ci/evidence -plan` to see what T1 will select. Add `-race` locally
  whenever the change touches concurrency, as AGENTS.md requires, whether or not the
  registry marks the domain `race: true`; that flag only decides what CI escalates.
- Do not run the whole `go test ./...` before every pull request, and do not rerun
  a passing suite without a source change or a specific diagnostic reason. Run it
  to investigate a failure or to validate a broad interaction. GitHub's run is the
  authoritative evidence; a local run is informal.
- Four things are separate: **PR acceptance** (impact-directed evidence and
  independent review), **merge** (authorized integration into `main`), **T2**
  (exact-revision trust eligibility, produced by the push to `main`) and
  **controller adoption** (a separate governed act following `trusted_main`).
  Independent pull requests need not wait for each other's T2; dependent changes
  still preserve ancestry and integrate on the right base. A failed T2 is a
  remediation item, never a reason to bypass verification.
