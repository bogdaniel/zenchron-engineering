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

The complete context is assembled from runtime-owned and machine-observed
facts: the producing run's identity, candidate commit/tree and contract, the
producer's agent id, CI state and existing review metadata for the **exact**
head, and the exact diff. Issue/PR/review-comment text travels as a clearly
delimited, labelled, untrusted block (`UNTRUSTED-REVIEW-CONTEXT`) the same way
an upstream stage's diff already does elsewhere in this runtime - it is data a
reviewer reasons about, never an instruction this system acts on, and an
attempt to forge the frame marker from inside that text is neutralized.

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

Publication to GitHub is idempotent: an already-published decision is a
no-op, a decision bound to a superseded head is refused before any GitHub
call, and the crash window between GitHub accepting a review and the local
publication record committing is closed by observing GitHub's own state
through the same self-identity primitive (`ForgeViewer`) the feedback
admission gate already relies on - reconciling from what GitHub shows rather
than submitting a second review.

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

**Binding design decision (#233 authority, stated here because it governs how
#474 may consume this port):** #474 MUST route a `REQUEST_CHANGES` decision to
producer remediation by reading `ReviewOutcome.Decision.Verdict` directly off
this port - never by waiting for the published GitHub review to pass ordinary
human feedback admission (`docs/github-feedback.md`). That admission gate's
self-loop guard deliberately refuses a review authored by this runtime's own
publishing identity, including a `REQUEST_CHANGES`, to stop the runtime
feeding itself. Where the reviewer's publishing identity is the same principal
that opened the producer PR - the common case absent a deliberately
provisioned, distinct reviewer-publishing identity - a published review would
be refused by that very guard and never reach the ordinary feedback path at
all. The durable `Decision`, read through `ReviewPort`, is the authorized,
producer-routable signal; GitHub publication is for human visibility and is
never the channel #474 consumes decisions through. This does not authorize
cross-WorkGraph chat or a second remediation engine: #474 still owns all
scheduling, routing and remediation-invocation logic, reading only typed,
durable state through this one port.

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
  review is one bounded attempt.

See also: [`github-feedback.md`](github-feedback.md), [`agents.md`](agents.md),
[`planning.md`](planning.md), [`product-architecture.md`](product-architecture.md).
