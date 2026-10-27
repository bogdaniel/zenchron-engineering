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
| `ZENCHRON_E2E_NO_SANDBOX` | `1`, read only by `scripts/browser_e2e.mjs`, which then adds `--no-sandbox` to the Chrome it starts itself (see below) |
| `ZENCHRON_E2E_ARTIFACTS` | `/artifacts/<test name>`, on a size- and inode-limited tmpfs inside the container (no host path is writable) |

`/candidate` stays read-only. A selected test that passes but wrote no artifact
did not demonstrably drive a browser and is inconclusive.

**Artifacts are bounded where they are written and where they are read.** Code the
candidate influences runs in the container, so `/artifacts` is a tmpfs capped at
32 MiB and 1024 inodes: the kernel refuses the write that would exceed it, and the
test that made it fails. No host path is writable. A real run writes more
screenshots than the output channel (8 MiB) can carry, so the runtime-owned end of
the workload exports two tiers on stdout (the image needs GNU `find`, `sha256sum`,
`awk`, `tar` and `base64`):

- a **manifest** of every regular file the tests wrote, path and SHA-256, with a
  count of everything that is not a directory so that a file the manifest cannot
  name is visible. At most 512 files, plain names only, every one under
  `<selected test>/`. The record's `artifacts_sha256` is the digest of the manifest,
  and "every selected test wrote something" is read from it;
- a **bounded set of the files themselves** for review: each test's share of 64
  files and 3 MiB, at most 1 MiB per file, as a tar.

The controller turns these into host state itself: regular files only, no
duplicates, no links or devices, every retained file listed in the manifest and
hashing to the manifest's value, each created exclusively under directories checked
to be real and hashed as it is copied. The full manifest is kept beside the
retained files (`MANIFEST.sha256`). A set that breaks a bound or a rule (an unnamed
file, a link, an over-limit or mismatched file) is refused as a whole; because that
is something the exercised code did, it is a **judged failure** with a failed record,
never a pass, whatever the tests reported. An export that cannot be read at all
(missing, cut off, undecodable) concludes nothing and is inconclusive. In a
representative run the tests wrote 83 files and 31 of them (2.7 MB) were retained.

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
- the workspace is built on the controller host from candidate-controlled content,
  so it is built the way the adopted builder builds a tree: from Git **objects**
  (`ls-tree -z` and `cat-file`, each blob verified against its object id), never from
  an archive or a checkout, so `.gitattributes` (`export-ignore`, `export-subst`,
  `eol`) changes nothing. The final file set is composed first - the candidate's
  entries minus the surface and minus any entry that stands where a surface
  directory belongs, plus the base's surface - and written once into an empty
  directory with every parent checked to be a real directory, so a candidate
  symlink or file at `cmd/zenchron-engineering`, `scripts` or `cmd` cannot redirect
  a write or a delete out of the scratch workspace;
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
above, plus `tar` and `base64`. The operator builds the image; the runtime assumes
nothing else. The workload checks them and exits 3 if any is missing, a prerequisite failure and never
a verdict.

**Node.** The scripts use the global `WebSocket`, so the image needs Node 22 or later.
Node 20 fails `TestBrowserWorkspace` with `WebSocket is not defined`.

**Chrome's own sandbox.** Chrome cannot start its sandbox inside the existing
isolation (no network, read-only root, all capabilities dropped, `no-new-privileges`,
default seccomp): it reports "No usable sandbox!", and as root "Running as root
without --no-sandbox is not supported". Two kinds of test meet this differently:

- The Playwright-driven tests launch Chromium with `--no-sandbox` by default, so
  they run in the boundary as it is.
- `scripts/browser_e2e.mjs` starts Chrome itself. It adds `--no-sandbox` **only**
  when `ZENCHRON_E2E_NO_SANDBOX=1`, and the verifier sets that variable **only** in
  the browser-assurance container. Every other launch keeps Chrome's sandbox. The
  container, which is the boundary, is unchanged: network off, `/candidate` and the
  root read-only, capabilities dropped, `no-new-privileges`, default seccomp, and the
  only writable mounts are the runtime-owned artifact directory and the tmpfs the
  base already provides. Nothing widens it (`--privileged`, `--cap-add`, a seccomp
  profile, a host namespace).

The tests that assert this: the opt-in variable is present in the browser container
and absent from the dependency-preparation and Go-suite containers; the container
keeps every restriction and gains no widening flag; and the real script is run
against a stand-in Chrome to show it keeps the sandbox by default, with
`ZENCHRON_E2E_NO_SANDBOX=0`, and adds the flag only with `1`.

`TestBrowserAcceptanceInARealContainer` is the opt-in end-to-end run (it needs Docker,
an image with the three paths above, and an operator module cache).

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
