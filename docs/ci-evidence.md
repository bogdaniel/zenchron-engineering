# CI evidence tiers

Issue #516. Merging code is an integration event; trusting it for adoption is
an authorization decision. CI evidence is split into tiers so that ordinary
pull requests do not pay for the whole repository suite, without any evidence
disappearing.

| Tier | What | When | Where |
|------|------|------|-------|
| T0 | `gofmt`, `go vet`, Windows build and vet | every PR | `ci/static.sh` |
| T1 | every package except `runtime` whole, plus the `runtime` tests the impact registry selects | every PR | `go run ./ci/evidence` |
| T1 race | the selected tests of high-risk (`race: true`) domains under `-race`, as a parallel job | every PR | `go run ./ci/evidence -race` |
| T2 | `go test -timeout 30m ./...` | today: every PR and every push to `main` (the required `go` check) | `.github/workflows/ci.yml` |
| T3 | `go test -race ./...` | nightly, on request | `.github/workflows/assurance.yml` |

## Status

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
ADR before the PR gate can drop the full suite (stages 4, 5 and 7).

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

The 25 most recent merged PRs, replayed through the selector:

- docs-only and CI-only PRs select only the guards;
- narrow fixes select 70–300 tests, about 1.5–3 minutes of CI time;
- the feature PRs of the #470/#472/#473/#492 kind each touch five to ten domains.
  They select 500–1100 tests, 4–7 minutes of CI time, which is still about half
  of today's full run;
- one PR in the 25 changed an unclassified dependency (`domain/`, `schemas/`)
  and failed safe to the whole package.

So the 2–5 minute target holds for narrow PRs. Wide PRs gain less, because they
legitimately need wide evidence.

## The impact registry

`ci/evidence/registry.json` is the one mapping from changed files to evidence.
The workflow has no path list of its own. Each domain names the source globs
whose change selects it, and the test-file globs (or test names) that make up
its evidence. `race: true` marks a high-risk domain, which also runs its tests
under `-race`. `always: true` marks the source-scan guards.

Selection is deterministic. Each changed path is handled by the first rule that
matches:

1. **Inert** paths, such as docs and Markdown, select no behavioural evidence.
2. A test file outside `runtime` is covered, because its package runs whole.
3. A changed `runtime` test file runs its own tests. A test file with no
   tests of its own is a shared helper, so it fails safe to the whole package.
4. A path matched by a domain's sources selects that domain.
5. An unclassified path in `runtime`, or in a package `runtime` imports, **fails
   safe** to the whole `runtime` package.
6. Any other path inside a package is covered by that package's whole run.
7. Anything else, such as `go.mod`, fails safe to the whole package.

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

- a `runtime` source file is not classified by any domain;
- a `runtime` test file is not reachable from any domain;
- a pattern matches no file, a named test does not exist, or a domain selects
  no tests.

So when a PR adds a file to `runtime`, the same PR has to classify it. Deleting
a rule leaves its files unclassified, and the conformance tests catch that.

A change in one domain can still break a test in another, through shared code.
T2 exists to catch that case. How often it happens is the measure of whether
the domains are drawn well.
