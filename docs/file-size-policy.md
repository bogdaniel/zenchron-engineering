# File-size policy as verified evidence

AGENTS.md section 4 caps source files (hard cap 1,000 lines, frozen baseline
ceilings, human-approved exceptions). That rule used to be checked only by a
local script nobody ran during a governed run, so two agent-delivered pull
requests (#652, #662) passed assurance and independent review while breaking it
(#663). It is now **verified evidence**: the controller checks the exact
candidate tree, a failure blocks the candidate, and the result is bound to the
commit, tree and contract like any other assurance result.

## What the controller does

When a run's compiled contract lists the required claim
`claim-file-size-policy` (evidence class `automated_test`), the baseline
verifier runs the size policy **before** the sandbox starts the Go suite:

- It reads the **exact candidate tree** (`git ls-tree` plus the blobs), on the
  host-side assurance checkout, so it needs neither a working tree nor the
  `.git` directory the sandbox masks.
- It applies the same rules as `scripts/check_file_sizes.py`: counted source
  suffixes, `vendor/`, `node_modules/` and `.venv/` excluded, generated-file
  header exempt, physical line count, hard cap 1,000, baseline ceiling,
  exception limit. A parity test runs both against the same repository.
- The ceilings and exceptions are read from the **base revision**, never from the
  candidate. A candidate that edits `file-size-baseline.tsv` or
  `file-size-exceptions.tsv` to pass is not honoured; it fails on the file it
  grew. The controller never writes either file.
- A refusal is a judged failure (`verification`), not an error. It takes the
  route of a failing test: the candidate does not pass assurance, so it cannot
  reach base integration, authority or publication, and the implementing agent
  is shown the sanitized report (path, line count, limit, and "move cohesive
  code into a new file; do not edit the manifests"). It costs milliseconds
  instead of a full suite run.
- The result is the assurance observation's own evidence, bound to the exact
  commit, tree, contract and policy revision. It is never an agent's statement.
- A size policy that cannot be read (missing base revision, malformed manifest)
  is a prerequisite failure, never a silently empty, permissive policy.

A contract compiled **without** the claim is verified exactly as before.

## The policy change the operator applies

The requirement exists only where `EngineeringPolicy` asks for it. Nothing in the
repository edits the operator's policy file. Add the following to **every rule**
whose effect publishes or accepts work (in the baseline policy, all six rules),
bump the policy `revision`, and restart the controller through the normal
succession so it loads the new revision:

```json
"required_claims": {
  "claim-file-size-policy": {
    "evidence_class": "automated_test",
    "independent_from_change_producer": true
  }
},
"obligations": {
  "file-size-policy": {
    "statement": "The repository's file-size policy must hold on the exact candidate tree.",
    "required_claims": ["claim-file-size-policy"],
    "material": true
  }
}
```

and append `"claim-file-size-policy"` to each rule's `authority_conditions[*].required_claims`
and to its `acceptance_discharge_claims`. (Keep each rule's existing
`required_claims`, `obligations` and the rest; these are additions.)

`policy/file_size_claim_test.go` applies exactly these additions to the fixture
policy, compiles it, and checks the claim, the obligation and the authority
condition all reach the contract.

## What is and is not frozen

A contract records the policy revision it was compiled under, and the claim
reaches a contract only through compiling under a policy that lists it. A
contract compiled before the change has no claim and is not checked. The runtime
does not, today, freeze a policy per run (a run's frozen policy covers budgets
and agent); a run is judged under the policy its serving controller loaded, so a
policy change takes effect for runs whose contract is recompiled after the
handover, as it does for every other policy change. This change adds no new
mechanism for that.

## Not covered here

Making `scripts/check_file_sizes.py` a required CI check is a separate
governance decision (ADR-0007) and is not made here. Staleness of baseline and
exception entries is a repository-maintenance rule enforced by the script, not a
property of a candidate, so it is not part of this gate.
