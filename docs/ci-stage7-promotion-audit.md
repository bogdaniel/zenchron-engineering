# ADR-0007 Stage 7 promotion audit

Issue #516. This audits the promotion gate in
[ADR-0007](adr/0007-trusted-main-from-revision-evidence.md) against what GitHub
actually recorded, so the decision to stop requiring the full-suite `go` check on
pull requests rests on observed runs, not on a merge count. It authorizes
nothing: the ruleset change is a separate operator act
([migration](ci-stage7-migration.md)).

Observed 2026-10-28, `main` = `a1ea63ec5a31845735da09a953c48d9a82da45b2`
(post-merge T2 run [40702392672](https://github.com/bogdaniel/zenchron-engineering/actions/runs/40702392672),
`success`). Raw rows: [`ci-stage7-audit/pull-request-observations.csv`](ci-stage7-audit/pull-request-observations.csv)
(485 completed pull-request workflow runs, each with its head SHA) and
[`ci-stage7-audit/merged-pull-requests.csv`](ci-stage7-audit/merged-pull-requests.csv)
(the 98 merged pull requests, their merge commits and their push-time T2 runs).

## Verdict

| Gate | Result |
|------|--------|
| At least two weeks of T0/T1 shadow | **Met**: 21 days (#520 merged 2026-10-07T22:29:53Z; observed to 2026-10-28) |
| At least 30 merged PRs | **Met**: 98 merged after #520 |
| Representative high-risk and multi-domain changes | **Met**: see Coverage |
| Every T1-PASS / `go`-FAIL case explained | **Met, with one stated limit**: 10 observations, all explained as flaky or runner timeouts; none is a selector miss. The limit: GitHub holds no second attempt on the same head for any of them, so "flaky" rests on the evidence below, not on a same-commit rerun |
| Flaky failures separated from selector misses | **Done**: 2 of 98 merge commits had a red T2, both flaky/infra |
| Controller runs the Stage 5 policy | **Met**: see Controller |

No promotion requirement is unproven. The residual risk is the flaky runtime
tests themselves (Findings), which affect T2 whether or not Stage 7 is applied.

## Coverage

Each merged PR's changed files, replayed through the **current**
`ci/evidence/registry.json` (the registry changed during the window, so domain
counts are an approximation; the historical T1 job results above are exact):

- 98 PRs; 21 fell back to the whole `runtime` package; 33 selected three or more
  domains; 20 selected no focused runtime evidence.
- High-risk domains: `controller` 25 PRs, `scheduler` 21, `store` 28,
  `handoff` 22; also `adoption` 24, `candidate` 26, `authority` 22,
  `workgraph` 22, `review` 22.

## T1 versus the full suite

Per completed pull-request run (485, 484 distinct head SHAs):

| | `go` PASS | `go` FAIL / cancelled |
|-|-----------|------------------------|
| **T1 PASS** (evidence + race aggregate) | 367 expected | **10 miss candidates** |
| **T1 FAIL** | 15 conservative blocks | 93 expected |

For the 98 **merged** PRs at the head that merged (the merge commit's second
parent): all 98 had `go` PASS; 95 also had T1 PASS and 3 had T1 FAIL. The three
(#532 head `0ba3f1e7`, #584 and #585 head `ecdbe5ce`) failed only the race
aggregate, and each was the pre-sharding single race job hitting its package
alarm (`panic: test timed out`, runs
[37742735304](https://github.com/bogdaniel/zenchron-engineering/actions/runs/37742735304),
[38303459493](https://github.com/bogdaniel/zenchron-engineering/actions/runs/38303459493)):
capacity, not a data race, and the reason T1 race now runs as four shards. 96 merge
commits have a green push-time T2 and 2 have a red one (below); every PR in the
window therefore has its own `go` result at the merged head and its T2 result on
the merge commit in the CSV.

### The 10 T1-PASS / `go`-FAIL observations

| Run | Head | `go` failure | T1 evidence on that head | Explanation |
|-----|------|--------------|-----------|-------------|
| [40668929651](https://github.com/bogdaniel/zenchron-engineering/actions/runs/40668929651) | `a5a609fe` | `TestSameGenerationWithoutTheLeaseHasNoAuthority` | whole `runtime` ran and passed (2208 tests under T1) | same commit, same test, different outcome: flaky |
| [39981229955](https://github.com/bogdaniel/zenchron-engineering/actions/runs/39981229955) | `dca145e9` | `TestALongToolIsSuspendedThroughoutAndClearsOnClose` | narrow: 350 tests | PR changed adopted-build/controller-status code; the test is provider suspension timing; it fails on unrelated PRs below |
| [39760145989](https://github.com/bogdaniel/zenchron-engineering/actions/runs/39760145989) | `ca2063c2` | `runtime` package alarm | narrow: 6 guard tests | 30-minute package alarm on a hosted runner (#656 raised it to 45m) |
| [39157650978](https://github.com/bogdaniel/zenchron-engineering/actions/runs/39157650978) | `4646e55c` | `TestNestedVerificationNativeShimAndCandidateRunShareCapacity` | 6 guard tests only | the PR changed only PNG files under `docs/acceptance/`; it cannot cause a runtime failure |
| [39075179632](https://github.com/bogdaniel/zenchron-engineering/actions/runs/39075179632) | `ff48c009` | same nested-verification test | narrow: 353 tests (orchestration, workgraph, remediation, planning); the test belongs to `verification`, which the PR did not touch | the same test failed on the docs-only PR above, so it fails with no code cause |
| [38593937196](https://github.com/bogdaniel/zenchron-engineering/actions/runs/38593937196) | `29c690ef` | `runtime` package alarm (30m) | whole `runtime` passed (1528 tests) | runner timeout |
| [38309725113](https://github.com/bogdaniel/zenchron-engineering/actions/runs/38309725113) | `34251229` | `TestTheSilentProviderFixtureReallyHasALiveDescendant` (0.20 s) | whole `runtime` passed (1280 tests) | flaky |
| [37848253929](https://github.com/bogdaniel/zenchron-engineering/actions/runs/37848253929) | `aaaaab6f` | `TestNestedVerificationKilledWrapperRetainsCapacityThroughGuardCleanup` | whole `runtime` passed (1452 tests) | flaky; the same test failed T2 on #686 |
| [37748697530](https://github.com/bogdaniel/zenchron-engineering/actions/runs/37748697530) | `b3935128` | `TestALongToolIsSuspendedThroughoutAndClearsOnClose` | narrow: 89 tests | PR changed trust/adoption code; unrelated provider timing test |
| [39157842462](https://github.com/bogdaniel/zenchron-engineering/actions/runs/39157842462) | `57485949` | `go` **cancelled** | n/a | superseded by a newer push; not a failure |

Four of the failures ran the **whole** `runtime` package under T1 on the same
commit and passed, so a missing selection cannot explain them. The narrow ones
failed in tests whose domain the PR did not touch, and the timing tests
(`TestALongTool…`, `TestNestedVerification…`) recur across unrelated PRs and
both red T2 runs. #689 (`6556a03`, `2f87b41`) hardened two of those timing
assumptions after the second red T2. No selector or registry correction was
needed.

### The two red post-merge T2 runs

| PR | Merge commit | Run | Cause |
|----|--------------|-----|-------|
| #654 | `99109a65` | [39766934504](https://github.com/bogdaniel/zenchron-engineering/actions/runs/39766934504) | `runtime` package hit its 30-minute alarm (`panic: test timed out`); its own PR `go` had passed. Runner capacity, fixed by #656 (45m) |
| #686 | `0d831736` | [40183206738](https://github.com/bogdaniel/zenchron-engineering/actions/runs/40183206738) | `TestAHealthyLongToolOpenedMidSlotIsNeverShownApproachingTheKill` ("not within 100ms") and `TestNestedVerificationKilledWrapperRetainsCapacityThroughGuardCleanup`; the PR changed no runtime file and its own `go` had passed. Timing flake, hardened by #689 |

Neither reached adoption: the resolver holds trust back at the previous accepted
revision, which is the behaviour ADR-0007 section 5 prescribes.

### Conservative blocks (T1 FAIL / `go` PASS): 15

11 race-aggregate failures and 1 race cancellation on heads where the plain
suite passed, and 3 `evidence` failures. They are reported, not gated: blocking
too much is the safe failure. Race-only failures are the evidence that
`evidence-race` finds something the non-race suite does not.

## Durations

Merged PRs at the merged head (n = 98; seconds):

| | p50 | p75 | p90 | max |
|-|-----|-----|-----|-----|
| `evidence` (T0 + T1) | 182 | 567 | 1320 | 1958 |
| slowest race part | 25 | 486 | 947 | 1810 |
| critical path, `evidence` ∥ race | 382 | 780 | 1320 | 1958 |
| `go` (full suite on the PR) | 1351 | 1527 | 1619 | 2199 |
| push-time T2 `go` (n = 96 green) | 1362 | - | 1606 | 1726 |

- `evidence` alone finishes in at most 5 minutes for 64 of 98 PRs, in 10 minutes
  for 75, and takes over 20 minutes for 13.
- With the race aggregate the critical path is at most 5 minutes for 46 of 98 and
  10 minutes for 61.
- The 21 PRs that fall back to the whole `runtime` package have a median
  critical path of 1227 s; the other 77 have 195 s. The long tail is the
  hotspot fallback (`runtime/operations.go`, #495), not the mechanism.

Redundant whole-suite executions: 535 completed pull-request `go` runs (all PRs,
including unmerged ones) for 98 merged PRs, plus one push-time T2 run per merge.
After Stage 7 the PR-time executions are zero and the push-time one remains.

## Controller

The serving controller is `main-a1ea63ec` (it advanced itself to the trusted main
after run 40702392672). Its adopted-build provenance carries `trust_evidence`
of kind `t2` for the exact commit (run 40702392672, integration 15368, workflow
`.github/workflows/ci.yml`, event `push`, branch `main`, job `go`, `success`),
and its status reports `update.trust.{floor, trusted_main, main_head}`. So the
Stage 5 resolver and the monotonic floor are deployed, and
`VerifyTrustRoot` requires no status check
([`runtime/trust_root.go`](../runtime/trust_root.go)): removing `go` from the
ruleset cannot stop adoption.

## Findings that are not promotion blockers

1. The flaky runtime tests above are an ongoing source of red T2 and red PR `go`
   (most recently `TestSameGenerationWithoutTheLeaseHasNoAuthority`, 2026-10-28).
   Stage 7 does not make them more likely; it makes a red T2 matter at merge
   rather than at the PR. Hardening them is separate work.
2. The replay uses today's registry. A selector miss under an older registry
   would show as a T1-PASS/`go`-FAIL row; none appears.
3. The conservative-block rate (15 of 485, 3.1%) is the price of failing safe.

## Method

`gh api repos/bogdaniel/zenchron-engineering/actions/workflows/ci.yml/runs?event=pull_request&created=>=2026-10-07`
for the runs, `…/actions/runs/{id}/jobs` for each job's conclusion and
`started_at`/`completed_at`, `gh pr list --state merged --search "merged:>=2026-10-07"`
for the merged PRs, `…/actions/workflows/ci.yml/runs?branch=main&event=push` for
T2, and `ci/evidence`'s `PlanEvidence` over each PR's files for coverage.
Durations are job wall time, not queue time.
