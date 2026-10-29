# CI evidence tiers

Issue #516. Merging code is an integration event; trusting it for adoption is
an authorization decision. CI evidence is split into tiers so that ordinary
pull requests do not pay for the whole repository suite, without any evidence
disappearing.

| Tier | What | When | Where |
|------|------|------|-------|
| T0 | `gofmt`, `go vet`, Windows build and vet | every PR | `ci/static.sh` |
| T1 | every package except `runtime` whole, plus the `runtime` tests the impact registry selects | every PR | `go run ./ci/evidence` |
| T1 race | the same selected high-risk (`race: true`) test names partitioned across four isolated runners; every part must pass | every PR | `go run ./ci/evidence -race -race-shard 1/4` (and 2/4–4/4) |
| T2 | `go test -json -timeout 45m ./...` | today: every PR and every push to `main` (the required `go` check); after Stage 7: every push to `main` only | `.github/workflows/ci.yml` |
| T3 | `go test -race ./...` | nightly, on request | `.github/workflows/assurance.yml` |

## Status

**Stage 7 is prepared, not active.** The promotion audit
([ci-stage7-promotion-audit.md](ci-stage7-promotion-audit.md)) finds the ADR-0007
gates met, the ruleset payloads and the ordered, reversible migration are in
[ci-stage7-migration.md](ci-stage7-migration.md), and
`ci/evidence/workflow_test.go` holds the parts of `ci.yml` the ruleset and the
trusted-revision resolver depend on. The live ruleset still requires `go` on
every pull request until an operator applies it. The remainder of this section
describes the state before that.


Stages 0–3 and the T3 sweep are in place. T0 and T1 run as the `evidence` and
`evidence-race` jobs beside the required `go` check. They are **not yet a merge gate**, and the full
suite has **not** moved post-merge.

That cannot happen in CI configuration alone. Adoption today treats the tip of
`main` as trusted because the `trusted-main-adoption` ruleset requires the
full-suite `go` check, strictly, on every merge (`DefaultTrustPolicy` in
`runtime/trust_root.go`). If that check stops being required, `VerifyTrustRoot`
refuses the ruleset and adoption fails closed. That is correct behaviour, but it
would stop adoption. Separating `main_head` from `trusted_main`, and letting
adoption consume `trusted_main`, changes the frozen M1-B trust root. It needs an
ADR before the PR gate can drop the full suite (stages 4, 5 and 7). That ADR is
ADR-0007, proposed in #528.

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
T1 race now partitions the **same sorted selected names** by position across four
isolated runners. Overlapping domains do not duplicate tests. Conformance tests
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
