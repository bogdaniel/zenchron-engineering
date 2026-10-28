# Stage 7 migration: fast pull-request CI, full post-merge assurance

Issue #516, [ADR-0007](adr/0007-trusted-main-from-revision-evidence.md). The
promotion evidence is in [ci-stage7-promotion-audit.md](ci-stage7-promotion-audit.md).
This document is the ordered, reversible procedure. **Nothing here is applied by
merging the preparatory change.** Steps marked OPERATOR change the live ruleset
or the workflow gate and need their own explicit authorization.

## Target configuration

| | Pull request | Push to `main` |
|-|--------------|----------------|
| T0 static (`ci/static.sh`) | `evidence` job | `go` job |
| T1 impact-directed tests | `evidence` job | - |
| T1 race (risk-selected, 4 shards) | `evidence-race-parts` → `evidence-race` aggregate | - |
| T2 full suite `go test -json -timeout 45m ./...` | **not run** | `go` job (unchanged identity) |
| T3 `go test -race ./...` | - | nightly / on request (`assurance.yml`) |

Ruleset `trusted-main-adoption` (id 22043609) requires, strictly and from GitHub
Actions (integration 15368), `evidence` and `evidence-race` instead of `go`.
Everything else in the ruleset is unchanged: pull request required, merge commits
only, no deletion, no non-fast-forward, no bypass actors.
[`ci/rulesets/trusted-main-adoption.pre-stage7.json`](../ci/rulesets/trusted-main-adoption.pre-stage7.json)
and [`…stage7.json`](../ci/rulesets/trusted-main-adoption.stage7.json) are the
exact before and after; `runtime/adopted_stage7_ruleset_test.go` holds them to
"valid adoption trust roots that differ only in the required checks".

What does **not** change: the `go` job's name, command and push-to-`main` trigger
(the identity `runtime/trusted_revision.go` accepts); the trusted-main resolver,
the monotonic floor and HOLD; controller adoption; branch integrity.

## Why the order matters

A required check that no workflow produces can never be satisfied, so no pull
request, including the one that fixes it, could merge (no bypass actor exists).
Two independent changes are involved, so there are four states:

| Ruleset requires | PR workflow produces | Mergeable? |
|------------------|----------------------|------------|
| `go` (before) | `go`, `evidence`, `evidence-race` (current) | yes - today |
| `evidence`, `evidence-race` (after) | `go`, `evidence`, `evidence-race` (current) | yes - `go` is advisory |
| `evidence`, `evidence-race` (after) | `evidence`, `evidence-race` (final) | yes - target |
| `go` (before) | `evidence`, `evidence-race` (final) | **no - deadlock** |

So the sequence only ever passes through the first three rows:

```text
activate:   [before, current] -> OPERATOR ruleset -> [after, current] -> merge workflow PR -> [after, final]
roll back:  [after, final]    -> merge revert PR   -> [after, current] -> OPERATOR ruleset  -> [before, current]
```

The workflow change is therefore a second pull request that must not merge while
the ruleset still requires `go`: until the ruleset changes, GitHub shows its `go`
check as expected and never reported, which is the correct signal. Its own
`evidence` and `evidence-race` jobs still run and must be green.

## Preconditions (re-observe; do not assume)

1. The serving controller runs the Stage 5 policy:
   `zenchron-engineering controller status --json` shows `update.trust.{floor,
   trusted_main, main_head}` and the active generation's provenance carries
   `trust_evidence.kind = "t2"`. (Verified on `main-a1ea63ec`.)
2. The promotion audit is current: no unexplained T1-PASS / `go`-FAIL case, and
   the newest `main` commit has a green exact-SHA T2.
3. The live ruleset equals the "before" payload (drift check):
   ```sh
   gh api repos/bogdaniel/zenchron-engineering/rulesets/22043609 \
     --jq '{name,target,enforcement,bypass_actors,conditions,rules}' \
     | jq -S . | diff - <(jq -S . ci/rulesets/trusted-main-adoption.pre-stage7.json) && echo identical
   ```
4. `evidence` and `evidence-race` have run on recent pull requests with
   `app.id = 15368` (the integration the ruleset will pin):
   ```sh
   gh api repos/bogdaniel/zenchron-engineering/commits/<recent-pr-head>/check-runs \
     --jq '.check_runs[] | select(.name=="evidence" or .name=="evidence-race") | [.name,.app.id,.conclusion]'
   ```

## Activation

1. **OPERATOR - apply the "after" ruleset** (reversible with step R2):
   ```sh
   gh api -X PUT repos/bogdaniel/zenchron-engineering/rulesets/22043609 \
     --input ci/rulesets/trusted-main-adoption.stage7.json
   ```
   Verify: re-read the ruleset; required checks are exactly `evidence` and
   `evidence-race`, strict, app 15368; `bypass_actors` empty; `controller status`
   still reports a trust root (adoption does not read the check list).
2. Merge the workflow pull request (`go` becomes push-only). It needs green
   `evidence` and `evidence-race` and the normal review; it is `BLOCKED` before
   step 1 and mergeable after it.
3. Observe the target configuration on the next ordinary pull request:
   - it merges with `evidence` and `evidence-race` only, and no `go` run starts;
   - the push to `main` starts the `go` job; until it succeeds `trusted_main`
     stays at the previous accepted revision and `main_head` moves ahead
     (`controller status --json`: `update.trust`);
   - when it succeeds, `trusted_main` advances and the controller updater may
     build a successor.
4. Acceptance: three consecutive ordinary pull requests merge on T0/T1 alone, one
   post-merge T2 is observed advancing `trusted_main`, and no run of `go` appears
   on a pull-request event.

## Rollback

Reverse order, so a valid merge path exists throughout:

- **R1.** Merge a revert of the workflow pull request. The ruleset is still the
  "after" one, which the restored workflow satisfies (it still produces
  `evidence` and `evidence-race`).
- **R2. OPERATOR.** Re-apply the "before" payload:
  ```sh
  gh api -X PUT repos/bogdaniel/zenchron-engineering/rulesets/22043609 \
    --input ci/rulesets/trusted-main-adoption.pre-stage7.json
  ```
  Only after R1 is on `main`; otherwise the deadlock row applies.

Rolling back never touches exact-revision adoption: T2 keeps running on every push
in every state, and `VerifyTrustRoot` accepts both rulesets.

If a red T2 appears on `main` after activation, nothing is rolled back: trust is
held at the previous accepted revision, the failure is remediated through the
ordinary issue → run path, and a later green covers it (ADR-0007 section 5).

## Development and review flow

- **PR acceptance** is the impact-directed evidence plus independent review.
- **Merge** is authorized integration into `main`; with Stage 7 it needs no
  full-suite run on the pull request.
- **T2** is exact-revision trust eligibility, produced by the push to `main`.
- **Controller adoption** is a separate governed act that follows `trusted_main`.

Independent pull requests do not wait for each other's T2: merging onto a head
whose T2 is pending is allowed, and `main_head` may run ahead of `trusted_main`.
A pull request that depends on another must contain it (merge ancestry), and a
dependent change should not be merged onto a base whose T2 has already failed
without addressing that failure. A failed T2 is a visible remediation item, never
a reason to bypass verification. Automatic merging is out of scope.
