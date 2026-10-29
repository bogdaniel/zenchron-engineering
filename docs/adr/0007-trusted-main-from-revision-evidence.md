# ADR-0007: Trusted Main Is Derived From Exact-Revision Evidence

- Status: Accepted
- Date: 2026-10-08
- Issue: #516 (CI evidence tiers). Stages 0–3 landed in shadow mode in #520.
  This ADR governs stages 4, 5 and 7, and the later evidence-reuse phase.
- Supersedes: the M1-B trust root as `DefaultTrustPolicy`
  (`runtime/trust_root.go`) defines it.

## The decision in one sentence

**`main_head` is an integration fact. `trusted_main` is an authorization
projection over exact-revision evidence. Neither the branch position nor a green
CI check is trust on its own.**

## Context

### What "trusted main" means in the code today

The M1-B trust root defines trusted main as three things together:

```text
trusted main
  = the current tip of refs/heads/main, as the forge reports it
  + a ruleset proving the branch moves only through merge-commit PRs:
    no bypass, no deletion, no non-fast-forward
  + a strict required status check "go" (GitHub Actions, app 15368)
```

`VerifyTrustRoot` refuses any ruleset that lacks the strict `go` requirement.
`BuildAdoptedController` and `ObserveTrustedMainRevision` then take
`observeTrustedMain`, the remote head of `main`, as the trusted revision:

- the build's default source is that head;
- containment is proven against it;
- it is re-observed before publication;
- it is recorded as `AdoptedBuildProvenance.TrustedMain`.

The `ControllerUpdater` polls that head and builds a successor every time it
moves. `controller_succession` admits a successor only when it is the exact
state of that head.

So "the newest commit on `main`" and "the newest commit that is trusted" are
the same value by construction. What justified that was a property of branch
movement: every merge happened under a ruleset that required a strict green
`go` on the pull request. It was never an exact-SHA T2 observation of the
resulting merge commit. Nothing recorded one, and v1 provenance does not carry
one.

### Why that collapse now costs throughput

#516 measured the price (#520, `docs/ci-evidence.md`):

- the full suite takes 13–18 minutes on every PR, and `runtime` alone takes
  655 s of it;
- with strict checks, a PR whose base moves pays that again;
- impact-directed T0/T1 evidence gives ordinary PRs merge-eligible evidence in
  about 1.5–3 minutes of CI time.

T1 cannot replace the full suite as the PR gate while trust is defined as
"passed the full suite before merge." If the ruleset stops requiring `go`,
`VerifyTrustRoot` refuses it and adoption fails closed. That is correct
behaviour, and it shows the problem: `main` represents two concepts that should
never have been one.

## Decision

### 1. Split the trust policy into branch integrity and revision trust

`TrustPolicy` becomes two policies with separate jobs.

```text
BranchIntegrityPolicy        what may move refs/heads/main, and how
  ref                       = refs/heads/main
  pull request required     = true
  allowed merge methods     = [merge]
  deletion                  = prohibited
  non-fast-forward          = prohibited
  bypass actors             = none, and the forge must disclose that it is none

TrustedRevisionPolicy        which revision on that branch is trusted
  evidence tier             = T2 (the full repository suite)
  subject                   = the exact commit SHA, on the first-parent chain of main
  producer                  = fixed check identity (see §4)
  accepted conclusion       = success
```

`RequiredCheck` and `RequireStrictChecks` leave the branch-integrity proof.
`VerifyTrustRoot` keeps every refusal it has today except the two about the
required check. It **adds no replacement check requirement**. Which checks a PR
must pass before merging (T0/T1) is merge policy, not trust. A renamed CI job
must never be able to break adoption, and the trust proof never rested on a
PR-time check of a merge preview in the first place.

This keeps the strongest property of M1-B: immutable merge-commit ancestry
that no one can bypass. Containment stays the proof that a source is in governed
history. What changes is which point in that history adoption may stand on.

Both policies are frozen data, exactly as `DefaultTrustPolicy` is today. No
caller can weaken either one and still get an artifact labelled adopted, and
both are recorded in the provenance.

### 2. `trusted_main` is resolved from main's own first-parent lineage

```text
observe main_head from the forge, after branch integrity is verified

walk the first-parent chain from main_head, newest first:
    R = the first commit with ACCEPTED T2 evidence whose subject is exactly R

trusted_main = R
```

- **First-parent only.** On a merge-only branch the first-parent chain is the
  sequence of states `main` actually held. A green check on a side-branch
  commit, a PR head, or a commit reachable only through a second parent is
  never a candidate. That holds even when that commit is an ancestor of
  `main_head`.
- **Bounded walk, failing closed.** The walk inspects at most **256**
  first-parent commits, a value frozen in `TrustedRevisionPolicy`:
  - `main` gained 117 first-parent commits in the 14 days to 2026-10-08, and 22
    on its busiest day, so 256 covers more than two weeks of red or pending T2
    at the observed rate;
  - a red streak longer than that is an incident to remediate, not a trust
    question to search further for;
  - the walk stops at the first accepted revision, so its normal cost is a
    handful of observations, not 256.

  If no accepted revision is found within the bound, there is no trusted
  main. A build with no previous trusted main to stand on refuses, with a
  typed error that names the bound. A running controller enters the §5 hold
  instead. Neither path ever falls back to `main_head`. Changing the bound
  changes `TrustedRevisionPolicy`, and so its recorded digest.
- **Pending and failed are both "not accepted".** The walk skips them. It
  records why it skipped each one, so the answer explains itself (§5).
- **A later green covers earlier failures.** If C3 fails T2 and C4 passes, C4's
  run exercised a tree that contains C3. C3 never needs to become trusted on
  its own.

```text
C1 ── C2 ── C3 ── C4 ── C5           main_head = C5
T2✓   T2✓   T2✗   T2✓   pending      trusted_main = C4
```

### 3. Adoption follows `trusted_main`, never `main_head`

```text
BuildAdoptedController:
    verify branch integrity              (refuse exactly as today)
    observe main_head
    resolve trusted_main                 (§2; refuse if none)
    fetch trusted_main
    default source = trusted_main
    prove source ⊆ trusted_main          (ancestry, as today)
    build, probe, measure
    before publishing: re-verify integrity and re-resolve trusted_main;
        the source must still be contained in the re-resolved trusted_main
```

- `ObserveTrustedMainRevision` returns the resolved `trusted_main`, and its
  name finally says what it does. The `ControllerUpdater` and
  `controller_succession` consume it unchanged. So **`main_head` moving never
  triggers a successor build; only `trusted_main` advancing does.** A merge can
  land immediately without a controller adopting code whose full assurance is
  still running.
- An explicit `request.Revision` must still be contained in `trusted_main`.
  Pointing at a newer, unassured `main` commit is refused, exactly as pointing
  at an unmerged commit is refused today.
- The pre-publication recheck keeps its current shape. The tree it compares
  against is the re-resolved `trusted_main`, not the re-observed head.

### 4. Evidence identity is part of the proof

The resolver reads T2 evidence as an observation, never as a boolean:

```text
T2EvidenceObservation
  subject.revision       exact commit SHA        must equal the candidate R
  subject.tree           recomputed from R       never taken from the forge
  tier                   "T2"
  check name             "go"
  producer integration   15368 (GitHub Actions)
  workflow               .github/workflows/ci.yml, event = push, branch = main
  check run id, attempt  as reported
  conclusion             success | failure | cancelled | ... | pending
  completed_at           as reported
  observed_at, observed_by (credential role and method, as for the trust root)
```

`TrustedRevisionPolicy` decides whether an observation is sufficient. These
rules apply:

- **The subject must be exact.** A check whose `head_sha` is not R is not
  evidence about R. A matching branch name or PR number is irrelevant.
- **The producer must be pinned.** A check named `go` from another app, from
  another workflow, or from a pull-request event does not count. Requiring the
  push event on `main` excludes PR merge previews by construction.
- **The latest completed attempt decides current eligibility, deterministically.**
  A re-run is a new attempt of the same check, on the same exact revision.
  - If its conclusion is success, R is eligible. If it is anything else, R is
    not.
  - A pending re-run does not override the latest completed attempt.
  - "Any attempt succeeded" is not the rule. Neither is "every attempt
    succeeded".
  - No human trust-override state exists or is introduced.
- **Every completed attempt stays observable.** The observation carries every
  completed attempt's id, number and conclusion, not only the deciding one.
  When completed attempts on the same revision disagree, R is marked
  **inconsistent**. That mark does not change eligibility. It is how flakiness
  becomes visible:
  - provenance and status show it;
  - promotion-gate metrics count it as flaky, not as a selector miss;
  - a later green restores eligibility without erasing the red that came
    before it.

CI produces evidence and policy authorizes. GitHub Actions is the producer, not
the trusted-main database. Nothing is stored as "trusted": `trusted_main` is
recomputed from forge observations each time it is needed, and the result is
recorded in the provenance of whatever was adopted.

`AdoptedBuildProvenance` moves to schema `adopted-build/2` and stops implying
that `main_head` and `trusted_main` are the same:

```text
main_head:      {revision, tree}             observed at publication
trusted_main:   {revision, tree}             resolved at publication
trust_root:     branch-integrity proof       ruleset id, digest, policy, bypass disclosure
trust_evidence: T2EvidenceObservation        about trusted_main exactly
skipped:        [{revision, reason}]         first-parent commits between main_head
                                             and trusted_main, and why they were not trusted
source:         {revision, tree}
containment:    source ⊆ trusted_main
```

`adopted-build/1` records stay readable and are never rewritten. A v1 record is
projected faithfully to the old M1-B model, and nothing more:

```text
main_head      = trusted_main = v1.trusted_main
trust_root     = v1.trust_root      (including the strict required-go policy it was verified under)
trust_evidence = legacy             (explicitly absent: no exact-SHA T2 observation was made)
skipped        = []
```

The projection must not synthesize a `T2EvidenceObservation`. M1-B's guarantee
was that the branch moved under a strict required-`go` ruleset. It was not that
the resulting commit itself had produced T2. The v2 model is deliberately
stronger on subject binding, and v1 records are not upgraded into claiming it.

### 5. Trust advances monotonically, and a failure holds trust back without rewriting anything

```text
A T2✓   B T2✓   C T2✗   D pending      main_head = D   trusted_main = B
                        D T2✓          main_head = D   trusted_main = D
```

A T2 failure at C does not:

- revert `main` or rewrite its history;
- invalidate B, or anything already adopted from B;
- rewrite earlier T0/T1/T2 evidence or adopted provenance;
- make C trusted;
- stop anyone merging D.

It does:

- leave `trusted_main` at the newest accepted revision;
- become visible: `doctor` and controller status report `main_head`,
  `trusted_main`, and the skip reason for each commit between them;
- stay actionable, as remediation work routed through the ordinary
  issue → run path. Nothing new is invented for it.

`trusted_main` **never moves backwards**. The monotonic floor is the
**previous trusted-main fact**, the `trusted_main.revision` recorded in the
running controller's adopted provenance. It is not the controller's
`source.revision`:

- an adopted controller may be built from an explicit older source that is
  contained in a newer trusted main;
- a resolver answer that is newer than that source, but older than the
  recorded trusted main, is still a regression.

For a v1 provenance the floor is the projected `trusted_main` above.

The updater compares each resolution with the floor:

```text
resolved == floor, or a first-parent descendant of it   ordinary: advance or no-op
resolved is an ancestor of the floor                    regression → HOLD
no accepted revision within the bound                   → HOLD
resolved not comparable (floor not on first-parent main) → HOLD
```

**HOLD** is a visible, degraded updater state with deterministic behaviour:

- **no successor is built or admitted** while it lasts;
- ordinary engineering service continues: `serve`, scheduling and runs under
  the current controller are unaffected;
- `doctor` and controller status report it with the floor, the resolved
  revision, the reason, and the T2 observations behind it;
- it **clears automatically** as soon as a resolution reaches the floor or a
  first-parent descendant of it, for example after a forward commit's T2
  passes. No acknowledgement step exists.

A human is needed only when monotonic trust cannot be restored that way, for
example if the floor itself is no longer on `main`'s first-parent chain, which
branch integrity should make impossible. The resolver never "repairs" trust,
and the updater never adopts backwards.

### 6. Evidence staleness: the model now, the implementation later

PR evidence (T0/T1) is bound to:

```text
PR head SHA, integration subject (base SHA), selected domain closure,
registry digest (ci/evidence/registry.json)
```

| Event | Effect on existing PR evidence |
|-------|-------------------------------|
| PR head changes | stale: old evidence never authorizes a new head |
| base moves; the intervening changes are disjoint from the PR's domain closure | may remain valid |
| base moves; the intervening changes intersect the closure or its dependencies | affected domains stale |
| base moves with a change the registry cannot classify | stale (fail safe) |
| manual conflict resolution or semantic rebase | affected domains stale |
| registry digest changes | stale |

This ADR fixes those semantics. It does **not** require implementing them
before stages 4 and 5. Until domain-aware reuse exists, GitHub's strict
up-to-date requirement stays on. Strictness then costs a T0/T1 rerun of about
2–5 minutes instead of 13–18. Strictness and the PR gate are two semantic
changes, and they are not made in the same step.

## Rollout

```text
#520  stages 0–3 + T3, shadow mode           merge after normal review
  │       live T0/T1-vs-go measurement begins
  ▼
this ADR accepted                              trust split decided
  ▼
stage 4  exact-SHA post-merge T2               already produced: ci.yml runs `go` on push to main;
                                               the observer for §4 is the new work
  ▼
stage 5  trusted_main resolver + adoption      §1–§5 in runtime, adopted-build/2,
                                               deliberate breaks below
  ▼
promotion gate satisfied                       see below
  ▼
stage 7  ruleset: required PR checks become `evidence` (and `evidence-race`,
         if the open question below resolves that way);
         `go` stops being required (it keeps running on push as T2);
         strict up-to-date stays on
  ▼
later    domain-aware evidence reuse (§6), then strictness may be relaxed
```

The runtime change in stage 5 is deployable **before** stage 7. While `go` is
still required on every PR, every first-parent commit on `main` has an accepted
push-event `go` run, so `trusted_main == main_head` in practice. The new
resolver therefore goes live while it still agrees with the old model, and that
agreement is itself evidence.

Stages 5 and 7 must not reach the ruleset in the wrong order. If `go` stopped
being required while the deployed controller still runs the old
`VerifyTrustRoot`, adoption would fail closed. That is safe, but it is an
outage. Stage 7 is gated on the deployed controller running the stage 5
policy.

### Promotion gate (before stage 7)

T0/T1 run in shadow beside `go` until both of these hold:

- **at least two weeks have passed, and at least 30 merged PRs have been
  observed**, whichever comes later;
- the observed PRs include representative high-risk changes (controller,
  scheduler, store, handoff) and multi-domain changes.

Every PR lands in one quadrant:

| | `go` PASS | `go` FAIL |
|-|-----------|-----------|
| **T1 PASS** | expected | **miss: blocks promotion until explained** |
| **T1 FAIL** | conservative false block | expected, high signal |

- Every T1-PASS/`go`-FAIL case must be explained, and the registry corrected,
  before promotion. "Explained" means either a domain mapping is fixed, or the
  case is shown to be a flaky full-suite failure.
- Flaky full-suite failures are classified separately, with the re-run
  evidence. They are not counted as selector misses, and they are not ignored
  either.
- The false-block rate is reported, but it does not gate promotion: blocking
  too much is the safe failure.

## Deliberate breaks (stage 5 is incomplete without these)

Each is a mutation that an automated test must catch:

1. the resolver accepts a revision whose T2 evidence `head_sha` differs from
   the revision (subject binding);
2. the resolver accepts a green T2 on a second-parent or side-branch commit
   (first-parent lineage);
3. the resolver returns a commit with failed T2 (failure holdback);
4. a newer accepted revision does not advance `trusted_main` (advancement);
5. adoption defaults to `main_head` (the `main_head`/`trusted_main`
   distinction);
6. no accepted revision within the bound falls back to `main_head`
   (fail closed);
7. a check named `go` from another producer, workflow or event is accepted
   (producer pinning);
8. `trusted_main` moves backwards and succession proceeds (monotonicity);
9. the monotonic floor is taken from `source.revision` instead of the recorded
   `trusted_main.revision`. The test is a controller built from an older
   source, plus a resolution between that source and its trusted main;
10. a revision whose latest completed T2 attempt failed is accepted because an
    earlier attempt passed, or a later passing attempt is not accepted
    (rerun rule);
11. disagreeing completed attempts are not marked inconsistent
    (flakiness visibility);
12. the v1 projection synthesizes a `T2EvidenceObservation` (legacy honesty);
13. HOLD does not clear when a forward resolution reaches the floor, or HOLD
    stops scheduling runs (hold semantics);
14. `VerifyTrustRoot` still requires `go` (old policy leak), and still refuses
    bypass, deletion, non-fast-forward or non-merge methods (integrity kept).

#520 already covers the T1-side breaks: registry mapping, unknown fallback,
T1 invariant, and high-risk escalation. Windows and non-Unix coverage stay in
`ci/static.sh` (T0) and are unchanged.

## Consequences

- Merging becomes cheap and adoption stays as strict as it is today. Adoption
  is now strict about the actual merged revision, not about a PR-time merge
  preview, which is arguably stronger than today's guarantee.
- Successor builds happen when assurance completes, not when code lands. The
  updater's "trusted main moved while building" supersession now fires on
  assured advances only.
- `main` can be red at its head. The red is visible, attributable and held
  back; it is never adopted. Developers merging onto a red head is allowed and
  is not the system's concern. It becomes the concern of remediation.
- Provenance gains a schema version. v1 records keep their meaning.
- The T2 definition lives in the tree it judges (`ci.yml`). A PR that weakens
  the `go` job weakens T2 for its own merge commit. That is the same exposure
  as today, where a PR can edit the check that gates it. This ADR does not
  close it. Required review of CI changes is the control, and a pinned T2
  command digest is a possible later tightening.

## Open questions

- Whether `evidence-race` should be a required PR check at stage 7, or only
  T2-adjacent. This is merge policy, not trust semantics, so stage 5 does not
  depend on it. Shadow data on its duration decides. The current estimate is
  2–6 minutes for narrow PRs and 10–20 minutes for wide ones; see
  `docs/ci-evidence.md`.
