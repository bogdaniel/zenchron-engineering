# Browser acceptance as verified evidence

A passing Go suite does not prove the dashboard works in a real browser. The claim
`claim-browser-acceptance` (evidence class `automated_test`) is discharged only
by a typed `browser_acceptance` record on the `assurance.observed` payload, written
when the verifier ran the repository's browser-driving tests against the **exact
candidate** and every one of them passed. It follows the file-size claim's
design (`docs/file-size-policy.md`): the record, not a green suite, is the proof.

## What the verifier does

When a run's compiled contract lists the claim, the baseline verifier runs, **after
the Go suite passes**, a second container from the same pinned image: no network,
read-only root, all capabilities dropped. It runs the tests named by
`browser-acceptance-tests.txt` (one Go test name per line), each on its own, and
reads `go test -json`. Each test gets the environment it needs to run instead of
skipping and a private writable artifact directory:

| Variable | Value |
|---|---|
| `ZENCHRON_BROWSER_E2E`, `ZENCHRON_REACT_BROWSER_E2E` | `1` (the skip gates of the selected tests; a source-scan test keeps this list in step with them) |
| `ZENCHRON_PLAYWRIGHT_MODULE` | `/opt/zenchron-browser/playwright/index.mjs` |
| `ZENCHRON_CHROME_EXECUTABLE`, `ZENCHRON_E2E_CHROME` | `/opt/zenchron-browser/chrome/chrome` (the two names different runners read) |
| `ZENCHRON_E2E_ARTIFACTS` | `/artifacts/<test name>`, a runtime-owned directory bind-mounted writable |

`/candidate` stays read-only. A selected test that passes but wrote no artifact
did not demonstrably drive a browser and is inconclusive. The record names the
candidate commit and tree, the **base** revision whose manifest and tests ran, the
checker and its revision, the digest of the selection, the digest of the base test
sources that ran (`surface`), the per-test results, the environment
(`docker-network-none@<image digest>`), the transcript and the digest of every
artifact the tests wrote. The browser run is part of the same assurance operation,
so it spends the same assurance attempts, wall allowance and confirmation rerun as
any other verification; observing again never adds attempts or resets a limit.

## Candidate-controlled tests

The tests are part of the candidate, so a candidate could weaken, empty or replace
them and still look verified. They are therefore **not the candidate's**:

- the list of tests comes from `browser-acceptance-tests.txt` **at the run's base
  revision**, like the file-size manifests, and a manifest that is missing, empty,
  oversized or holds anything but a plain test name refuses verification;
- the workspace the container sees is the candidate's exact tree with the test
  surface replaced by the base revision's: `cmd/zenchron-engineering/*_test.go` and
  everything under `scripts/`. The candidate's versions of those files, and any
  test file it adds there, are discarded; the application under test (the Go
  packages and the embedded frontend bundle) stays the candidate's;
- a base test that no longer compiles against the candidate is a judged failure.
  Change the tests in their own reviewed change first, then the work that needs
  them.

`TestACandidateCannotWeakenTheBrowserTestsItIsVerifiedBy` builds a candidate that
turns a browser test and a script into no-ops and adds a test of its own, and shows
the workspace holds the base's test, the base's script and none of the added file.

## Feature-specific acceptance

The generic selection proves the existing workspace flows still work. A change
that adds a user-visible feature, such as the #693 delivery report, needs
acceptance for **that** feature as well, and the candidate cannot supply it
(above). The route:

1. A reviewed change lands the feature's browser test and its line in
   `browser-acceptance-tests.txt` **before** the feature work starts. It can be
   written against the specification by someone other than the implementer.
2. Work based on that revision runs the new test as part of the claim, against the
   final integrated candidate (the exact tree being published, because the
   container runs the candidate's tree), with the base's test sources.
3. If the feature's test cannot exist before the feature, the generic claim does
   not cover it. The browser run on the integrated candidate is then supervisor
   evidence, recorded as such and reviewed by a person; it is never presented as
   the automated claim.

## What the image must provide

The pinned `assurance.image` must contain Node, Playwright and Chrome at the paths
above. The operator builds the image; the runtime assumes nothing else. The
workload checks them and exits 3 if any is missing, a prerequisite failure and never
a verdict.

**Observed in a representative container** (Debian-based `golang:1.25` plus Node 20,
`playwright-core` and Debian Chromium at the paths above, run through the verifier
in the unchanged isolation: no network, read-only root, all capabilities dropped,
`no-new-privileges`, default seccomp; `TestBrowserAcceptanceInARealContainer` is the
opt-in test):

- All four selected tests execute and none skips; every one writes its artifacts
  through the writable `/artifacts` mount while `/candidate` stays read-only; the
  record carries the exact commit, tree, base, surface and artifact digests.
- Chrome cannot start with its own sandbox in that boundary: it can create neither
  its setuid nor its user-namespace sandbox ("No usable sandbox!", and as root
  "Running as root without --no-sandbox is not supported"). The Playwright-driven
  tests (`TestDashboardDescribedObjective`, `TestDashboardFeatureWalkthrough`,
  `TestReactEngineeringOSGovernedWorkspace`) nevertheless run, because Playwright
  launches Chromium with `--no-sandbox` unless asked not to. `TestBrowserWorkspace`
  starts Chrome itself (`scripts/browser_e2e.mjs`) without that flag and fails with
  the root-sandbox error. It therefore cannot pass in this boundary as written.
- `TestReactEngineeringOSGovernedWorkspace` failed once in three runs on a timing
  assertion (`Reading runtime state…` still on screen), and passed in the others.
  An intermittently failing selected test makes the claim intermittently
  undischargeable and spends assurance attempts.

The runtime adds no browser flag and does not wrap the image's browser. Whether to
change `TestBrowserWorkspace`'s launch, drop it from the manifest, give the browser
container a different profile, or accept the browser's own sandbox being off while
the container stays the boundary, is an operator decision that needs its own
authorization. Until then the claim cannot be discharged with the current
manifest, and it fails closed.

## What is, and is not, evidence

| Outcome | Result |
|---|---|
| every selected test passed, exit 0 | a passing record; the claim can be discharged |
| a test failed, or the build failed (exit 1 or 2) | a **failed** record; the candidate is judged and the transcript is shown for remediation |
| a test skipped | inconclusive: no record, assurance prerequisite failure |
| a passing test that wrote no artifact | inconclusive |
| a selected test produced no result, or no output at all | inconclusive |
| no browser, Playwright or Node in the image (exit 3) | inconclusive |
| killed container, unavailable sandbox, any other exit | inconclusive |

Inconclusive never becomes passing evidence. A record proves the claim only for the
exact commit and tree it names, so a changed candidate loses it; the record must
also name the current checker revision and selection, so a change to either makes
the check eligible to run again. The record is optional and additive: observations
written before it existed, and every observation from a generic suite, prove
nothing about a browser, and replay never invents one.

Operator acceptance stays distinct: this claim is `automated_test`, produced by the
verifier. A person's `human_approval` claim is a different claim from a different
producer, and neither substitutes for the other.

## The policy change the operator applies

The requirement exists only where `EngineeringPolicy` asks for it; nothing in the
repository edits the operator's policy. Work that does not touch the user
interface must keep its existing behavior, so the requirement is attached to a
single rule that matches only when the interface changed.

1. In the project model, add a boundary so the analysis produces the fact
   `user_interface.boundary_modified` (any boundary `type` yields
   `<type>.boundary_modified`):

```json
"critical_boundaries": {
  "user_interface": {
    "type": "user_interface",
    "paths": ["frontend/src/**", "controlplane/assets/**"]
  }
}
```

2. In the policy, add one rule and bump the policy `revision`. It adds a **material
   obligation** and no authority condition: a second rule's condition on the same
   action would be refused as conflicting, and a material obligation's claims
   already gate every protected action (`authority.requiredClaimsFor`), so
   publication waits on the claim.

```json
"USER-INTERFACE-TOUCHED-001": {
  "when": { "fact": "user_interface.boundary_modified", "equals": true },
  "effect": {
    "required_claims": {
      "claim-browser-acceptance": {
        "evidence_class": "automated_test",
        "independent_from_change_producer": true
      }
    },
    "obligations": {
      "browser-acceptance": {
        "statement": "The dashboard must pass its real-browser acceptance tests on the exact candidate.",
        "required_claims": ["claim-browser-acceptance"],
        "material": true
      }
    }
  }
}
```

`policy/browser_acceptance_claim_test.go` applies exactly this rule to the fixture
policy, compiles with the fact true and false, checks that the obligation reaches
only the first contract, and that publication is held with the claim missing.

A contract compiled before the change keeps its identity and has no such claim:
nothing is retroactive. When the contract newly requires the claim, the assurance
operation binding gains `|browser-acceptance@<checker revision>`, so the missing
verification is planned by the ordinary scheduler instead of stranded.
