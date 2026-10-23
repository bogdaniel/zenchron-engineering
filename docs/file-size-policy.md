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
- The manifests are validated exactly as `scripts/check_file_sizes.py` validates
  them, and a manifest it refuses is refused here, naming the manifest and the
  entry: two entries for one path (the later one never silently widens an
  approved limit), a path that is absolute, has a `..`, `.`, empty or trailing
  component, a wrong column count, an empty field, a limit that is not a
  canonical integer above 1,000, an exception for a baseline file that does
  not state a limit above its ceiling, and an entry (in either manifest) that is a
  symlink. The symlink test uses the pinned candidate tree's own listing (git
  mode 120000), never a filesystem path a producer could have shaped; not
  counting a symlink as a source file is a separate rule. Parity tests run the real script and the
  Go verifier on the same manifests and require the same refusal. The script's
  *stale-entry* rules (an entry for a path that is no longer a source file over
  the cap) are repository maintenance, not a property of a candidate, and are
  not part of this gate.

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

## The claim is proven by its own recorded check

A passing automated-test observation proves what the Go suite covers. It does
**not** prove the file-size policy was checked, so it never discharges
`claim-file-size-policy`. The verifier records each specialised check it
completed as a typed record on the `assurance.observed` payload
(`checks`, an optional bounded list; see
[spec/runtime-v0.1.md](spec/runtime-v0.1.md)):

```json
{"kind": "file_size_policy", "checker": "file-size-policy-go",
 "checker_revision": "sha256:…", "outcome": "passed",
 "commit": "<exact candidate commit>", "tree": "<exact candidate tree>",
 "base_revision": "<governing base the rules were read from>"}
```

When the evidence bundle is rebuilt, the claim is discharged **only** if the
current passing observation carries a successful record by the current checker,
bound to the observation's own commit and tree and to the contract's governing
base. Its evidence names the host-side checker as producer
(`deterministic_tool`) and `controller_host` as the environment, not the sandbox
verifier. Every other claim a generic passing suite supports is discharged exactly
as before.

Before the check reads anything, the verifier proves the assurance checkout's
HEAD is the exact recorded commit and tree (the checkout was cloned from the
verified subject store); a checkout that is not the exact candidate is a
prerequisite failure, never a verdict.

### Fail closed

- An observation written before the record existed carries none, and absence is
  **never** read as proof. Replay does not synthesize a record; earlier passing
  observations keep discharging the claims they always discharged.
- A record that names another commit, tree, base, checker or checker revision, a
  failed outcome, or another kind proves nothing.
- A contract that requires the claim for a run with **no base revision** refuses
  verification. It is never skipped.

### A newly required check is planned, not stranded

The `assurance.go` operation binding is `commit | tree | contract revision`. When
the loaded policy lists the claim and the rebuilt contract requires it, the
binding gains `| file-size-policy@<sha256(checker revision NUL base)>`: the FULL
SHA-256 of the complete checker revision and base, 64 hex characters. It is a
digest, not the raw values, so the binding has a fixed length (it also appears in
operation and event identifiers, and the repository bounds payload fields), and
it is the full digest, never a prefix, so two distinct revisions that share a
prefix still change it. An assurance that
ran before the requirement existed no longer satisfies that binding, and neither
does one recorded by an older checker: a release that changes the checker's
revision changes the binding too, because the claim accepts only a record by the
current checker and the candidate would otherwise be stranded as "already
assured". An unchanged revision keeps the binding stable, so nothing re-runs
needlessly. The scheduler then plans the verification again for the exact
subject (no second scheduler). It runs
under the run's ordinary assurance attempt and wall budgets: when they are
exhausted the run settles with the existing bounded failure, and no budget is
renewed. A run whose contract does not require the claim keeps the binding it
always had.

### Compatibility and rollback

`checks` is additive and optional, so every existing journal stays readable and
valid. Event payloads are decoded strictly, so a controller built **before** this
change cannot read an `assurance.observed` event that carries `checks`: roll back
only past a point where no such event has been written, or hand over to a build
that includes the member.

## What is and is not frozen

A contract records the policy revision it was compiled under, and the claim
reaches a contract only through compiling under a policy that lists it. A
contract compiled before the change has no claim and is not checked. The runtime
does not, today, freeze a policy per run (a run's frozen policy covers budgets
and agent); a run is judged under the policy its serving controller loaded, so a
policy change takes effect for runs whose contract is recompiled after the
handover, as it does for every other policy change. For such a run the claim can
be discharged only by a fresh observation that carries the record, which the
binding change above causes to be planned. This change adds no new mechanism for
freezing a policy.

## Not covered here

Making `scripts/check_file_sizes.py` a required CI check is a separate
governance decision (ADR-0007) and is not made here. Staleness of baseline and
exception entries is a repository-maintenance rule enforced by the script, not a
property of a candidate, so it is not part of this gate.
