# Independent PR review

`docs/github-feedback.md` is the loop from a human review back to the worker
that produced the change. This document is the other half: a first-class
**independent review role**, with its own identity, that inspects an exact PR
head and reaches a durable, GitHub-publishable decision - without the operator
manually gathering the run, candidate, evidence, CI state and comment history
into a hand-assembled prompt for another agent.

## What it is not

It is not implementation, not remediation, and not merge or release authority.
A reviewer may read, run independent checks inside its own read-only
workspace, and produce a verdict; it never gains candidate-mutation, push or
merge authority merely because it can write code.

## Operator surface

```bash
zenchron-engineering autonomy review pr 232 --agent claude [--publish]
zenchron-engineering autonomy review status 232 [--text]
```

`--agent` names the reviewing worker, resolved through the same operator
agent registry every other command uses. `--publish` is the separate
authority statement for sending the resulting decision to GitHub as a review;
performing a review and publishing it are two different permissions, and the
first never implies the second.

`review pr` resolves the producing `EngineeringRun` automatically from this
runtime's own `github.pr_observed` journal history - never from the PR itself
- and refuses explicitly, as `*ReviewSubjectUnboundError`, when no run has
ever observed that pull request number. It never invents provenance from a PR
description.

## The review packet

Assembled from runtime-owned and machine-observed facts: the producing run's
identity, phase and disposition, candidate commit/tree and contract, the
producer's agent id, CI state and existing review metadata for the **exact**
head, and the exact diff (read from the content-verified subject store, never
from the review workspace directly - see #437 in `docs/architecture.md`).
Issue/PR/review-comment text travels as a clearly delimited, labelled,
untrusted block (`UNTRUSTED-REVIEW-CONTEXT`) the same way an upstream stage's
diff already does elsewhere in this runtime - it is data a reviewer reasons
about, never an instruction this system acts on, and an attempt to forge the
frame marker from inside that text is neutralized.

Not yet in the packet: assurance/authority-decision references and the
run's full execution/remediation history. These are deferred, not silently
dropped - the packet never claims to carry them, and a reviewer judging an
obligation it cannot verify from what it was given reports that explicitly as
a finding rather than assuming it passed (see the reviewer contract below).

## The reviewer contract

The reviewer is told, in the runtime's own words, that it did not author the
change and must not infer correctness merely because CI is green, tests pass,
another agent produced it, this runtime authorized publishing the PR, or the
PR's own description claims the issue is solved. It answers with exactly one
structured verdict - reusing the same wire protocol and slot-file channel a
plan-stage reviewer already uses (`ReviewerResult`: `schema_version`,
`verdict`, `findings`, `reason`), extended with one additive, optional
`severity` (`blocking` / `non_blocking`) per finding.

## Decision and publication

The reviewer's two-value verdict (accept/block) and each finding's severity
together produce one of three durable decisions:

| Verdict | Reached when |
| --- | --- |
| `APPROVE` | accepted, no findings |
| `COMMENT_ONLY` | accepted, only non-blocking findings - or GitHub refused the configured identity's approval |
| `REQUEST_CHANGES` | blocked, at least one blocking finding |

A decision is bound to the **exact** head it was reached against
(`review.Subject`). If the PR moves, the old decision stays durable historical
evidence and is reported `stale`; a fresh review is required for the new head.
`review status` answers exactly this: the latest decision, blocking/
non-blocking finding counts, and whether it is stale.

Publication to GitHub is idempotent and matched precisely: every published
review body carries an immutable, invisible decision-id marker, and
reconciliation (after a crash, or after a client-side error that may have
masked a GitHub success) matches on that marker plus this runtime's own
publishing identity - never on identity alone - so one decision's publication
can never be mistaken for another's, even when the same identity has
published several reviews to the same pull request. A decision bound to a
superseded head is refused before any GitHub call. GitHub's own, textually
confirmed refusal of a self-approval is the one case that downgrades an
`APPROVE` to an explicit `COMMENT_ONLY`; every other failure - a timeout, a
5xx, a rate limit, an auth failure, or any other 422 - is preserved for
replay.

Concurrency is exclusive, not advisory: performing a review, and separately
publishing one, each claim a durable key before any expensive or
externally-visible work, so two concurrent callers for the same exact
subject and reviewer can never both invoke a provider or both reach GitHub.
A losing caller is refused immediately with a typed conflict rather than
blocking; a claim is released on ordinary completion and reclaimed after a
bounded staleness window if its owner crashed.

## The #474 interface

`runtime.ReviewPort` (`RequestReview` / `LatestDecision` / `IsStale` /
`NeedsReview`) is the complete, narrow surface #474's automatic WorkGraph
review/remediation progression is expected to consume. #233 owns the review
operation itself; #474 owns deciding when to call it and how to route a
`REQUEST_CHANGES` decision back into producer remediation. Nothing in this
file schedules anything or holds a lease.

`RequestReview` returns a `ReviewOutcome{Decision, Published, PublishedVerdict}`,
not a bare `Decision`: #474 needs to know not just what was decided but what
actually happened on GitHub, without re-deriving it.

**Design decision #233 is making about its OWN port (#474 still owns whether
and how to act on it):** a `REQUEST_CHANGES` decision is never reachable by
waiting for the published GitHub review to pass ordinary human feedback
admission (`docs/github-feedback.md`). That admission gate's self-loop guard
deliberately refuses a review authored by this runtime's own publishing
identity, including a `REQUEST_CHANGES`, to stop the runtime feeding itself -
and where the reviewer's publishing identity is the same principal that
opened the producer PR (the common case absent a deliberately provisioned,
distinct reviewer-publishing identity), a published review is refused by that
very guard and never reaches the ordinary feedback path at all. So #233
exposes `ReviewOutcome.Decision.Verdict` directly through `ReviewPort`,
unconditionally on whether GitHub publication succeeded, refused, or was
never attempted - that is as far as #233's authority reaches.

**What this is not:** reading `Decision.Verdict` off this port is not, by
itself, authorization to run a remediation provider. #474 remains the sole
owner of deciding whether and how a `REQUEST_CHANGES` decision becomes an
authorized, exact-subject, anti-self-loop, budget-governed remediation
invocation - with its own tests for that admission - exactly the way it
already owns all scheduling and routing logic. This document cannot grant
#474 authority it does not independently establish; it only commits #233 to
exposing the fact #474 needs in order to establish it.

## Known scope limits (first version)

- `review pr` runs directly in the invoking process; routing it through a
  running supervisor's control plane the way `orchestrate`/`workgraph` do is
  not wired yet.
- The review workspace is materialized from a local Git source (the same
  mechanism and reachability assumption `CreatePlanningWorkspace` already uses
  for planning): the exact PR head must already be reachable from it. Fetching
  an arbitrary PR head on demand is not solved here.
- Findings carry no file/line location yet, so GitHub publication always uses
  the review body, never inline comments.
- No per-review budget/retry policy is wired to operator configuration; a
  review is one bounded attempt (concurrency is still exclusive, see below).
- The review packet omits assurance/authority-decision references and the
  run's full execution/remediation history; see "The review packet" above.
- A reviewer-publishing identity distinct from the producer's own publication
  identity is an operator provisioning decision (a second credential/App),
  not something this code enforces or defaults; see "The #474 interface"
  for why that identity choice matters.

See also: [`github-feedback.md`](github-feedback.md), [`agents.md`](agents.md),
[`planning.md`](planning.md), [`product-architecture.md`](product-architecture.md).
