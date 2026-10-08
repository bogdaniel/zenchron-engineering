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
  uses for an upstream candidate reference) and `AssertIntegrity`. There is no
  second Git engine and no second candidate/commit path.
- **The `FailureWorkspaceIntegrity` recovery route.** `RestoreTrusted` is the
  runtime's existing, operation-provenance-gated recovery for a workspace
  whose Git metadata no longer matches its trusted baseline
  (`runtime/operations.go`'s `restore`). `IntegrateInputs` never calls it on
  an unexplained divergence itself - see "What is new" below - so a workspace
  a crash left mid-plan is refused, and recovered only through that existing
  route, exactly like any other candidate operation's integrity violation.
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

1. Proves `ws` was actually created at the contract's base revision, aborts
   any leftover **conflicted** merge (which never moves HEAD, refs or
   config, so this alone can never hide a real divergence), and then checks
   `ws.AssertIntegrity()`. Anything else that moved HEAD, refs or config away
   from the trusted baseline - a crash after an earlier plan step already
   committed, or genuine tampering - is **refused**, not silently restored:
   `IntegrateInputs` never calls `RestoreTrusted` on an unexplained
   divergence itself. Recovery is the runtime's existing
   `FailureWorkspaceIntegrity` route, with real operation provenance; once
   that has run, a fresh attempt proceeds and is not a second, competing
   integration of the same input set.
2. For each input in canonical order: re-verifies, against a LIVE lookup
   (`IntegrationSources`, never the contract itself), that the unit's
   currently admitted handoff still matches the one the contract recorded. A
   superseded commit can remain perfectly fetchable; only this comparison
   proves it is still current. Then it fetches the exact committed object
   from the producer's own runtime-owned workspace, proves its tree matches
   what was admitted, and proves the contract's base is actually an ancestor
   of it (the base is **verified**, not merely stated - and "not an
   ancestor" is never guessed at: a Git failure here is not silently
   collapsed into "no," it is returned as a real error unless Git's own
   documented, silent "no" exit is what happened).
3. The commit is merged. A merge failure is classified from Git's OWN
   repository state, not from the mere presence of an error: it is a
   conflict only if Git left unmerged paths (`git diff --name-only
   --diff-filter=U`, read before the merge is aborted) - that is what a
   conflict IS. A merge that failed for any other reason (a dirty working
   tree, a bad object, an environment failure) is aborted and its real cause
   returned as an error, never mislabeled a conflict. A real conflict
   discards everything THIS call composed so far, not merely the failing
   step: the workspace returns to the verified base it started at, so a
   blocked attempt is never read as having partly applied an earlier input.
   The reported `integration.Conflict{Kind: ConflictTextual}` names exactly
   the conflicted paths - never guessed at, and never auto-resolved:
   composition never edits a conflicted hunk itself, so no producer's
   admitted output is silently changed to make another's fit.
4. A clean result is `integration.Integrated`, a `Result` naming the new
   exact `Revision`, `Tree` and the consumed input set's digest - never one
   of the inputs' own commits.
5. An input this attempt cannot read at its admitted subject - an unknown
   unit, a superseded handoff, an unreadable or tree-mismatched object, or a
   commit that is not a descendant of the verified base (a replaced upstream
   subject) - is `integration.Invalidated`, never silently skipped or
   guessed at.

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
conflict cannot use that opening to touch unrelated material. `Conflict`'s
paths are never truncated to fit a bound: `NewConflict` **refuses** a
conflict shaped larger than the bound outright, because `VerifyRemediationScope`
reuses `Paths` as the exact in-scope set - a silently truncated list would
silently narrow a producer's real, legitimate material or silently claim a
conflict was fully described when part of it had been dropped from the
record.

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
