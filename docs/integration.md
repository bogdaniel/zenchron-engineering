# Integration (#475)

Two parallel `WorkGraph` (#472) units can each produce a correct local
candidate that still conflicts with the other, textually or in what the two
changes mean together. Nothing makes two admitted branches become one product
change by itself. `integration.Contract` and `runtime.IntegrateInputs`
(package `integration`, adapter in `runtime`) are the deterministic
composition responsibility that closes that gap:

```text
upstream WorkUnit A -> admitted handoff A
upstream WorkUnit B -> admitted handoff B
                  |
     integration WorkUnit's ordinary EngineeringRun
                  |
       fresh governed candidate workspace, at a VERIFIED base
                  |
       ordered, deterministic merge of A then B (by unit id)
                  |
    one exact new commit/tree  -or-  a typed, bounded conflict
                  |
       (same as any other commit) fresh admission, assurance, review
```

## What it reuses, unchanged

- **The WorkGraph itself.** An integration attempt's consumed inputs ARE a
  WorkGraph unit's `WorkUnitInputs` (#472) - `integration.Contract.Inputs` is
  that type, not a second shape for the same fact. A unit with
  `depends_on: [a, b]` already receives exactly those two admitted outputs,
  recorded against the batch that owns its child run, the same way any other
  dependent unit does.
- **The batch/run identity.** `WorkUnitBatchID` already binds a unit's child
  run to its graph, its unit and its exact input set, so replaying a crashed
  activation finds the same run and never a second one. Integration adds
  nothing here: "no duplicate integration of the same exact input set" is
  this existing property, not new code.
- **Candidate/Git plumbing.** `IntegrateInputs` (`runtime/integration.go`)
  performs no Git operation this repository did not already have:
  `CreateCandidateClone`'s local, credential-free fetch between two
  runtime-owned workspaces (the same transfer `MaterializeCandidate` already
  uses for an upstream candidate reference), the merge/`ConflictError`
  vocabulary `IntegrateBase`/`Rebase` already use, and `RestoreTrusted`'s
  existing hard-reset recovery. There is no second Git engine and no second
  candidate/commit path.
- **Admission, assurance and review.** An integration attempt runs inside an
  ordinary governed `EngineeringRun`. Nothing here bypasses or duplicates
  handoff admission, reassessment or review: a clean composition is a new
  exact commit that still needs its own fresh evidence, exactly like any
  other run's commit. Evidence or review attached to input A or input B never
  authorizes the composed output - `integration.IntegratedCandidate` carries
  no evidence, review or acceptance field, and a regression test
  (`TestIntegratedCandidateCarriesNoInheritedEvidence`) guards that it stays
  that way.
- **Budgets, waits and authority.** Because this is an ordinary run, its
  attempt/provider-invocation budgets, external waits and merge/release
  authority are the existing scheduler's and the existing authority owner's.
  The integration coordinator grants none of that itself.

## What is new

`integration.Contract` (functional core, no runtime/store/Git import) names
one attempt: the graph, the unit, the verified base revision, and the
consumed `WorkUnitInputs`. `Contract.Plan()` is the deterministic composition
order - by consumed unit id, independent of arrival order, matching
`Contract.Digest()`'s own canonicalization.

`runtime.IntegrateInputs(ws, contract, sources)` performs that plan inside a
candidate workspace the caller already cloned at the contract's base:

1. Proves `ws` was actually created at the contract's base revision, then
   unconditionally returns it there (aborting any leftover merge first) -
   before the first step, and after every blocked one. A crash anywhere in a
   multi-input plan, or a conflict on any step but the first, is cleaned up
   the same way, so a restarted attempt always recomposes the whole plan from
   one clean base rather than compounding an abandoned one.
2. For each input in canonical order: fetches the exact committed object from
   the producer's own runtime-owned workspace and proves its tree matches
   what was admitted; proves the contract's base is actually an ancestor of
   it (the base is **verified**, not merely stated); merges it.
3. A Git conflict is read from Git itself (`git diff --name-only
   --diff-filter=U`) before the merge is aborted, and reported as a bounded
   `integration.Conflict{Kind: ConflictTextual}` naming exactly the
   conflicted paths - never guessed at, and never auto-resolved: composition
   never edits a conflicted hunk itself, so no producer's admitted output is
   silently changed to make another's fit.
4. A clean result is `integration.Integrated`, a `Result` naming the new
   exact `Revision`, `Tree` and the consumed input set's digest - never one
   of the inputs' own commits.
5. An input this attempt cannot read at its admitted subject - an unknown
   unit, an unreadable or tree-mismatched object, or a commit that is not a
   descendant of the verified base (a replaced upstream subject) - is
   `integration.Invalidated`, never silently skipped or guessed at.

`integration.ClassifyAssuranceFailure` is the typed vocabulary for the other
kind of conflict this package can observe but not detect by itself: a clean
merge whose independent assurance then fails. It never runs assurance and
never decides correctness; it only turns that caller-supplied verdict into a
`ConflictSemantic` (attributable to the composition) or `ConflictUncertain`
(not yet attributable either way) blocked result, refusing to classify an
assurance failure as `ConflictTextual`. Composition never upgrades an
uncertain result to "correct" on its own.

`integration.VerifyRemediationScope` refuses a remediation that changes a
path a textual conflict did not name, so a worker directed to resolve a
conflict cannot use that opening to touch unrelated material.

## Known limitations and follow-ups

- **Not yet wired into `serve`'s automatic reconciliation.** This ticket adds
  the deterministic composition capability and proves it against controlled
  repositories; it does not add an "integrator" unit kind to
  `runtime/workgraph.go`'s `activateWorkUnit`, and does not choose which role
  in #64's catalogue triggers `IntegrateInputs` instead of an ordinary
  provider invocation. That wiring is the natural extension point once #473's
  communication API and #480's objective-to-team compilation land; until
  then, `IntegrateInputs` is available to be called from a run's execution
  path the same way any other candidate operation is.
- **No #473 Finding/DecisionRequest emission yet.** `integration.Result`'s
  `Conflict` is already the complete typed content a Finding or
  DecisionRequest needs (kind, bounded detail, bounded paths); translating it
  into #473's actual message once that protocol stabilizes is a few lines at
  the call site, not a redesign here. Until then, a caller reads `Result`
  directly.
- **Semantic/uncertain classification has no automatic caller yet.** This
  ticket does not invent a second verification engine: whether a clean
  merge's combination broke something is answered by the existing
  assurance/reassessment pass against the integrated candidate, exactly as
  for any other run's commit. `ClassifyAssuranceFailure` is ready for that
  pass to call; this ticket does not itself decide when a failure is
  attributable to the integration versus pre-existing in one input alone.
- **Two-input shape.** `Contract` requires at least two inputs - a WorkGraph
  unit with a single dependency needs no integration attempt; #472's ordinary
  dependency delivery already covers it.
