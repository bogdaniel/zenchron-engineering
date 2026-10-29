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
| Every T1-PASS / `go`-not-PASS case explained (ADR-0007: a mapping fixed, or shown to be a flaky full-suite failure with re-run evidence) | **Evidence recorded, verdict reserved for the owner**: 10 observations (9 `go` failures, 1 cancellation); none shows a selector miss in the current replay (which cannot rule one out under an older registry, Findings). Re-run evidence: the `go` job of all four narrow-selection failures was **rerun on GitHub (attempt 2, owner-authorized 2026-10-29) and passed the full suite**; 3 further failures passed in the T1 whole-`runtime` job on the same head; the remaining 2 are package-alarm timeouts, one of which also passed in the T1 whole-`runtime` job. Original failures are kept as attempt 1 |
| Flaky failures separated from selector misses | **Done**: 2 of 98 merge commits had a red T2, both flaky/infra |
| Controller runs the Stage 5 policy | **Met**: see Controller |

The gates are met on the evidence stated, subject to the owner's evaluation of
the re-run evidence against ADR-0007 (hosted reruns below). The residual risk is
the flaky runtime tests
themselves (Findings), which affect T2 whether or not Stage 7 is applied.

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

Per completed pull-request workflow run that has the T1 jobs (485 runs, 484 distinct
head SHAs, one row per run at its latest attempt). The 50 earlier runs of the same
window (2026-10-07T10:38Z to 2026-10-08T06:17Z) predate the `evidence` jobs and are
excluded; all 535 runs counted `go` (see Durations). A failed or cancelled T1 job
makes T1 non-PASS; cancellation is kept apart from failure:

| | `go` PASS | `go` FAIL | `go` cancelled |
|-|-----------|-----------|----------------|
| **T1 PASS** (evidence + race aggregate) | 367 expected | **9 miss candidates** | 1 |
| **T1 FAIL** | 14 conservative blocks | 86 expected | 0 |
| **T1 cancelled** (no failed T1 job) | 1 | 0 | 7 |

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

### The 10 T1-PASS / `go`-not-PASS observations

Nine are first attempts that failed and were never rerun on GitHub, so no
GitHub-hosted retry outcome exists for them. The tenth (run 39157842462) is itself attempt 2 and
was cancelled. "Directly evidenced" below means the failing test **passed in a different job on
the same commit** (the T1 whole-`runtime` run), not that `go` was rerun.

| Run | Head | `go` failure | T1 evidence on that head | Class | Explanation |
|-----|------|--------------|-----------|-------|-------------|
| [40668929651](https://github.com/bogdaniel/zenchron-engineering/actions/runs/40668929651) | `a5a609fe` | `TestSameGenerationWithoutTheLeaseHasNoAuthority` | whole `runtime` ran and passed (2208 tests under T1) | directly evidenced flaky | that test passed in the whole-`runtime` T1 run on the same commit |
| [39981229955](https://github.com/bogdaniel/zenchron-engineering/actions/runs/39981229955) | `dca145e9` | `TestALongToolIsSuspendedThroughoutAndClearsOnClose` | narrow: 350 tests | hosted-rerun green (flaky) | attempt 2 of this run passed the full `go` job (locally the test also passed 10/10 idle and 80/80 under load); the PR changed adopted-build/controller-status code, the test is provider suspension timing, and it fails on unrelated PRs below | | `go test -json -timeout 45m ./...` |
| [39760145989](https://github.com/bogdaniel/zenchron-engineering/actions/runs/39760145989) | `ca2063c2` | `runtime` package alarm | narrow: 6 guard tests | timeout | 30-minute package alarm on a hosted runner (#656 raised it to 45m); a capacity failure, not a failing test, and no same-head full run exists |
| [39157650978](https://github.com/bogdaniel/zenchron-engineering/actions/runs/39157650978) | `4646e55c` | `TestNestedVerificationNativeShimAndCandidateRunShareCapacity` | 6 guard tests only | hosted-rerun green (flaky) | attempt 2 of this run passed the full `go` job (locally the test also passed 10/10 idle); the PR changed only PNG files under `docs/acceptance/`, so no code path can explain it | | `go test -json -timeout 30m ./...` |
| [39075179632](https://github.com/bogdaniel/zenchron-engineering/actions/runs/39075179632) | `ff48c009` | same nested-verification test | narrow: 353 tests (orchestration, workgraph, remediation, planning); the test belongs to `verification`, which the PR did not touch | hosted-rerun green (flaky) | attempt 2 of this run passed the full `go` job (locally the test also passed 10/10 idle and 80/80 under load); it belongs to `verification`, which the PR did not touch, and it failed on the docs-only PR above | | `go test -json -timeout 30m ./...` |
| [38593937196](https://github.com/bogdaniel/zenchron-engineering/actions/runs/38593937196) | `29c690ef` | `runtime` package alarm (30m) | whole `runtime` passed (1528 tests) | timeout | 30-minute package alarm; the whole `runtime` passed under T1 on the same commit |
| [38309725113](https://github.com/bogdaniel/zenchron-engineering/actions/runs/38309725113) | `34251229` | `TestTheSilentProviderFixtureReallyHasALiveDescendant` (0.20 s) | whole `runtime` passed (1280 tests) | directly evidenced flaky | that test passed in the whole-`runtime` T1 run on the same commit |
| [37848253929](https://github.com/bogdaniel/zenchron-engineering/actions/runs/37848253929) | `aaaaab6f` | `TestNestedVerificationKilledWrapperRetainsCapacityThroughGuardCleanup` | whole `runtime` passed (1452 tests) | directly evidenced flaky | that test passed in the whole-`runtime` T1 run on the same commit; the same test failed T2 on #686 |
| [37748697530](https://github.com/bogdaniel/zenchron-engineering/actions/runs/37748697530) | `b3935128` | `TestALongToolIsSuspendedThroughoutAndClearsOnClose` | narrow: 89 tests | hosted-rerun green (flaky) | attempt 2 of this run passed the full `go` job (locally the test also passed 10/10 idle); the PR changed trust/adoption code and the test is unrelated provider timing | | `go test -timeout 30m ./...` |
| [39157842462](https://github.com/bogdaniel/zenchron-engineering/actions/runs/39157842462) | `57485949` | `go` **cancelled** | n/a | cancelled | superseded by a newer push; not a failure |

Counts: 1 cancelled, 2 timeouts, 3 directly evidenced flaky, 4 hosted-rerun
green (flaky). In four rows the whole `runtime` package passed under T1 on the same
commit (three failing tests, one timeout), so T1 did not withhold those tests. That
does not establish that the selector mappings of the time were right for other PRs.

#### Re-run evidence for the four narrow-selection failures

**GitHub-hosted reruns.** The owner authorized `gh run rerun RUN_ID --failed` for
these four runs on 2026-10-29. Each rerun re-executed the whole `go` job (T0 and the full-suite step
`go test … ./...`, as the workflow stood at that run's merge commit) as **attempt 2
of the same run**; attempt 1, the
original failure, is unchanged. Attempt 2 checked out the pull-request merge
commit the run was created for (`refs/remotes/pull/N/merge` at the SHA below; for
run 39075179632 the logs of both attempts show the same SHA). Data:
[`ci-stage7-audit/historical-reruns.csv`](ci-stage7-audit/historical-reruns.csv).

| Run | Head | Merge ref SHA tested | Test that failed in attempt 1 | Attempt 1 `go` | Attempt 2 `go` | Attempt 2 wall time | Attempt 2 full-suite command |
|-----|------|----------------------|-------------------------------|----------------|----------------|---------------------|-------------------------------|
| [37748697530](https://github.com/bogdaniel/zenchron-engineering/actions/runs/37748697530) | `b3935128` | `f2375330` | `TestALongToolIsSuspendedThroughoutAndClearsOnClose` | failure | **success** | 17 m 54 s |
| [39981229955](https://github.com/bogdaniel/zenchron-engineering/actions/runs/39981229955) | `dca145e9` | `8d98b325` | `TestALongToolIsSuspendedThroughoutAndClearsOnClose` | failure | **success** | 28 m 02 s |
| [39075179632](https://github.com/bogdaniel/zenchron-engineering/actions/runs/39075179632) | `ff48c009` | `03996664` | `TestNestedVerificationNativeShimAndCandidateRunShareCapacity` | failure | **success** | 25 m 28 s |
| [39157650978](https://github.com/bogdaniel/zenchron-engineering/actions/runs/39157650978) | `4646e55c` | `22f6589e` | `TestNestedVerificationNativeShimAndCandidateRunShareCapacity` | failure | **success** | 25 m 34 s |

All four full-suite reruns passed on the same revision that had failed. The
command differs because each rerun uses the workflow at its own merge commit: three
ran the 30-minute bound (one without `-json`), only run 39981229955 ran the current
45-minute command. Every one ran the whole suite. What this shows: none of the four
failures is deterministic at its revision, which makes an intermittent full-suite
failure the explanation the evidence supports. What it does not show: *why* each failed the
first time, or that the selector mappings of the time were correct for these PRs. The tests are timing tests that recur across unrelated PRs and both red
T2 runs, so runner load is the likely cause, but a pass does not prove it.

**Local reruns (supplementary).** Before the hosted reruns, each failing test was
also run in a detached worktree at its exact head (`go test ./runtime -run
'^<Test>$' -count=10`: pass on all four) and, for `dca145e9` and `ff48c009`, as eight
concurrent processes of the compiled test binary, `-test.count=10` each (80 runs
per head, all exit 0). The CI failures did not reproduce locally.

The recurring timing tests (`TestALongTool…`, `TestNestedVerification…`) are the
common cause; #689 (`6556a03`, `2f87b41`) hardened two of those timing assumptions
after the second red T2. The current replay shows no selector miss; it cannot show
that none occurred under an older registry (Findings).

### The two red post-merge T2 runs

| PR | Merge commit | Run | Cause |
|----|--------------|-----|-------|
| #654 | `99109a65` | [39766934504](https://github.com/bogdaniel/zenchron-engineering/actions/runs/39766934504) | `runtime` package hit its 30-minute alarm (`panic: test timed out`); its own PR `go` had passed. Runner capacity, fixed by #656 (45m) |
| #686 | `0d831736` | [40183206738](https://github.com/bogdaniel/zenchron-engineering/actions/runs/40183206738) | `TestAHealthyLongToolOpenedMidSlotIsNeverShownApproachingTheKill` ("not within 100ms") and `TestNestedVerificationKilledWrapperRetainsCapacityThroughGuardCleanup`; the PR changed no runtime file and its own `go` had passed. Timing flake, hardened by #689 |

Neither reached adoption: the resolver holds trust back at the previous accepted
revision, which is the behaviour ADR-0007 section 5 prescribes.

### Conservative blocks (T1 FAIL / `go` PASS): 14

11 race-aggregate failures and 3 `evidence` failures on heads where the plain
suite passed (one further run, with a cancelled race aggregate, is counted
separately above). They are reported, not gated: blocking
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

Redundant whole-suite executions: 535 completed pull-request `go` runs (the 485
runs above plus the 50 that predate the `evidence` jobs; all PRs, including
unmerged ones) for 98 merged PRs, plus one push-time T2 run per merge.
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
2. Coverage and domain counts replay each merged PR's files through today's
   `ci/evidence/registry.json`; the registry changed during the window. The
   historical T1 job results are exact, but whether an older registry would have
   selected enough evidence for a PR is **not established**. A selector miss would
   have appeared as a T1-PASS / `go`-FAIL row, and none of the nine failures is
   attributable to one, but that is an observation, not a proof of historical
   selector correctness. Replaying each PR with the registry at its merge commit
   would close this.
3. The conservative-block rate (14 of 485, 2.9%; 3.1% counting the cancelled
   race aggregate) is the price of failing safe.

## Method

All commands quote their URLs (an unquoted `>=` is a shell redirect) and paginate.

```sh
gh api --paginate \
  'repos/bogdaniel/zenchron-engineering/actions/workflows/ci.yml/runs?event=pull_request&created=>=2026-10-07&per_page=100' \
  --jq '.workflow_runs[]'
gh api --paginate \
  'repos/bogdaniel/zenchron-engineering/actions/runs/RUN_ID/jobs?filter=all&per_page=100' --jq '.jobs[]'
gh pr list --state merged --limit 1000 --search 'merged:>=2026-10-07' \
  --json number,mergeCommit,mergedAt
gh api --paginate \
  'repos/bogdaniel/zenchron-engineering/actions/workflows/ci.yml/runs?branch=main&event=push&per_page=100' \
  --jq '.workflow_runs[]'
```

The jobs give each check's conclusion and `started_at`/`completed_at`; the merged
head is the merge commit's second parent, because `headRefOid` can be stale for a
merged PR. Coverage replays each PR's files through `ci/evidence`'s `PlanEvidence`.
Durations are job wall time, not queue time.
