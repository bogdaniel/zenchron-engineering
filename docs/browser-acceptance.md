# Browser acceptance as verified evidence

A passing Go suite does not prove the dashboard works in a real browser. The claim
`claim-browser-acceptance` (evidence class `automated_test`) is discharged only
by a typed `browser_acceptance` record on the `assurance.observed` payload, written
when the verifier ran the repository's browser-driving tests against the **exact
candidate** and every one of them passed. It follows the file-size claim's
design (`docs/file-size-policy.md`): the record, not a green suite, is the proof.

## What the verifier does

When a run's compiled contract lists the claim, the baseline verifier runs, **after
the Go suite passes**, a second container from the same pinned assurance image: no
network, read-only root, all capabilities dropped, the candidate mounted read-only.
It runs a fixed, runtime-owned selection of Go tests that drive Chrome through
Playwright against a real, isolated controller:

`TestBrowserWorkspace`, `TestDashboardDescribedObjective`,
`TestDashboardFeatureWalkthrough`, `TestReactEngineeringOSGovernedWorkspace`

and reads `go test -json`. The record names the candidate commit and tree, the
checker and its revision, the digest of the selection, the per-test results
(`total`, `passed`, `failed`, `skipped`), the environment (`docker-network-none@<image
digest>`) and the immutable transcript. Because the browser run is part of the same
assurance operation, it spends the same assurance attempts, the same wall
allowance and the same confirmation rerun as any other verification; observing
again never adds attempts or resets a limit.

## What the image must provide

The pinned `assurance.image` must contain Node, Playwright and Chrome at fixed
paths. The operator builds the image; the runtime assumes nothing else:

| Path | Purpose |
|---|---|
| `/opt/zenchron-browser/playwright/index.mjs` | `ZENCHRON_PLAYWRIGHT_MODULE` |
| `/opt/zenchron-browser/chrome/chrome` | `ZENCHRON_CHROME_EXECUTABLE` |
| `node` on `PATH` | runs the Playwright scripts |

An image without them cannot pass. The workload checks the three and exits 3, which
is a prerequisite failure and never a verdict. Whether Chrome can launch with every
capability dropped is a property of the image and its launch flags; a browser that
cannot start produces failing or skipped tests, and neither is evidence.

## What is, and is not, evidence

| Outcome | Result |
|---|---|
| every selected test passed, exit 0 | a passing record; the claim can be discharged |
| a test failed, or the build failed (exit 1 or 2) | a **failed** record; the candidate is judged and the transcript is shown for remediation |
| a test skipped | inconclusive: no record, assurance prerequisite failure |
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
