# #344 paired dogfood protocol, amendment v2 (#481)

Status: **DRAFT for owner review. Not a freeze, and not launch authorization.**
The owner approved drafting it on 2026-10-19 with the decisions in section 2,
including a corrected plan aggregate budget.
It becomes the frozen replacement only when the owner approves this file at an
exact PR head and it merges. Launching a measured arm needs a separate, later
owner authorization.

This file supersedes `docs/dogfood/344-paired-protocol.md` (v1) for the
controls named in section 2 and leaves v1 byte-for-byte unchanged. v1 blob
`3e41da871527167ff0e595474f20b3f0966abe31`, sha256
`c1f0ab31331e896504154dccdd8f37ae7eecf281adf05642c9b2a3c187e4eed1`.

Parent epic #471. Milestone #481. Measurement definitions #66. Owner decisions:
issue #481 comment 6251685466, and the owner decisions recorded on #639 and
#640 on 2026-10-19.

## 1. Why v1 is amended

No measured arm has validly started. After v1 froze, an assistant-driven test
run of #344 was made at the owner's explicit instruction. It deliberately
violated v1 sections 4 and 11, so it is **not** a valid Arm A and produces no
benchmark datum. It found two launch-blocking defects:

- #639: the operator could not plan, approve and review through the Product
  Workspace without a long, error-prone command sequence;
- #640: the frozen 30-minute per-run wall budget could not fit the repository's
  successful 43-minute full-suite assurance. The run (`run-de5b6172075e37120b6abd30cb907e12`)
  spent about 8 minutes executing and about 43 minutes verifying, then ended
  `failed run_wall_budget_exhausted` with its candidate held.

The test run's evidence is preserved and is not part of any arm. It is not
counted, averaged or hidden. A further nonblocking status inconsistency is #645.

## 2. What changes

Everything not listed here is unchanged from v1 and still binding: the question
(section 1), the task text and acceptance criteria (section 2), the operator
rules (section 4), logging (section 5), measurements (section 6), the four-hour
elapsed allowance and censoring (section 7), isolation (section 8), confounds
(section 9), audit (section 10), invalidation (section 11) and outputs
(section 12). The invalid test run does not count as an arm that "started".

| Control | v1 | v2 (proposed, for owner review) |
| --- | --- | --- |
| Base SHA, both arms | `b02b8a4369d11afce2d7c12d60ee69f1d1bf9769` | **The merge commit of the PR that merges this file into `main`.** Owner decision 2026-10-19: the planner has no explicit pinned-base argument and reads the revision of `main`, so the benchmark base is the revision the planner will actually use. The exact SHA cannot be written into this file (it does not exist before the merge, and writing it would change the file); it is recorded in a separate freeze record (section 2.3). Both arms clone exactly that revision. A docs-only descendant is **not** an acceptable substitute for exact equality |
| #344 code on base | none | none. To be verified on the merge commit before it is frozen (section 2.3). It was verified on `61b28f8` before drafting: no held-material release command, event or operation. The invalid test run's candidate branch was never published or merged |
| Instrumentation | `66-minimal-slice/1`, schema `0.2` | unchanged (`benchmark.InstrumentationVersion` at the new base) |
| Controller | plain `go build` of the base, Go `1.27.2` | **adopted** controller built by `controller build-adopted` from the frozen base (the merge commit, which must be trusted main with accepted post-merge `go` evidence), Go `1.27.2` darwin/arm64, run by the published artifact and sanctioned with `controller re-adopt`; `/v1/controller` must report `live_build.kind` `adopted`, `generation_match` `match`, `role` `held`, `serving` `serving` and `durable_consistency` `consistent`, with `source_revision` equal to the frozen base, captured at Arm A start. A plain `go build` is unattested: it records no durable generation and the Control Plane refuses every governed action, so it is not a valid measured arm |
| Execution budget per run | `wall_limit_seconds` 1800 | unchanged: 1800 s (attempt wall 30 min). It now bounds execution and other charged work only |
| Assurance budget per run | none (charged to the 1800 s) | new: a separate bounded **3600 s** cumulative assurance allowance, frozen per run at creation (`RunBudgets.AssuranceWallLimit`, `DefaultAssuranceWallLimit` = 1 hour, #640). Assurance is not unlimited: exhaustion ends the run `run_assurance_budget_exhausted` with the candidate held, and a timed-out or exhausted verification never satisfies assurance, authority or publication |
| Aggregate accounting | plan `max_wall_seconds` 14400 | **corrected to 28800** (owner decision 2026-10-19). Total active work is reported as execution plus assurance plus other charged work, and the plan aggregate still bounds all of it. 14400 would have been an unintended earlier bottleneck: it counts execution and assurance across every child run, and six runs each performing a 43-minute verification alone consume 15,480 s. 28800 s is 8 hours of summed active work, matching two concurrent workers over the four-hour elapsed allowance. It does not lengthen the four-hour allowance and does not guarantee success; verification capacity, serial execution and legitimate failures can still prevent completion |
| Plan envelope | `max_child_runs` 8, `max_concurrency` 2, `max_provider_invocations` 20, `max_wall_seconds` 14400, no `max_cost_micros` | `max_child_runs` 8, `max_concurrency` 2, `max_provider_invocations` 20, **`max_wall_seconds` 28800**, no `max_cost_micros`. The operator configuration `plan` block is the operator ceiling a proposed plan may not exceed, so it must equal these values at Arm A start (the machine's block currently holds 14400 and must be changed before any plan is proposed); a plan proposed under a different configuration is refused by the controller (`configuration_changed`). The compiler also divides the envelope wall budget by the planned agents to cap each stage, so the per-stage cap changes with this value; each run's own 1800 s and 3600 s limits still apply |
| Provider families and models | frozen resolved ids, operator-attested | unchanged: `codex` `gpt-6.1-sol`, `claude` `claude-sonnet-5-5`. They are re-captured from each CLI's first normal invocation per arm and a difference stops the arm. They remain operator-attested |
| Four-hour allowance, both arms | 4 h elapsed | unchanged |
| Runtime state, Arm A | not stated | a **fresh, empty state directory** for Arm A. The earlier state directory holds the invalid test run and rehearsal runs and is not reused |

What the 60-minute assurance allowance does **not** do: it leaves headroom over
one successful 43-minute verification but cannot hold two. A complete rerun is
not promised, and exhaustion is reported as an outcome. It does not make
verification faster and does not guarantee that a whole WorkGraph fits inside
the four-hour allowance.

### 2.1 Carried forward unchanged, stated explicitly

- Task and acceptance criteria: issue #344 exactly as v1 section 2. Issue body
  sha256 `d045a5e90188212846ac2705c4fccab0b4e36c59de45c4b68419e22c61d62054`,
  issue `updatedAt` `2026-10-03T18:28:36Z`. **Revalidated 2026-10-19** against
  the live issue: both values are unchanged. They are re-checked once more at
  the freeze, and a difference stops it.
- Operator: Bogdan Olteanu operates and times both arms.
- Reviewer independence: an independent reviewer of a different vendor family
  from the implementer, in both arms.
- Attempt limits per run: `max_execution_attempts` 2, `max_remediation_attempts`
  2, `max_assurance_attempts` 2.
- Comparison rules: one pair is one observation; no leverage ratio; the confounds
  in v1 section 9 stand.

### 2.2 Freeze record and single reference

The blob and content sha256 of this file are recorded in the PR freeze comment
at the exact approved head, as v1's were; a digest written into this file would
change the digest. **Both arms reference this same v2 freeze** (its recorded blob
digest, the same base SHA and the same budgets) in the first line of each arm's
operator log. An arm that references any other protocol text is invalid under
v1 section 11.

### 2.3 Freeze record, kept outside this file

After this file merges, a separate freeze record (a PR comment, as v1's was)
states: the merge commit SHA (the frozen base); a check that it contains no #344
implementation; the blob and content sha256 of this file at that commit; the
effective operator configuration (the `plan` block, `budgets`, models and trust
modes, with before and after values and a digest); and that `main` is held. Each
arm then verifies that the **actual plan subject and run base equal the frozen
SHA** and stops if they differ.

## 3. Preconditions before the owner is asked for launch authorization

These are not satisfied by this document. Each is separately evidenced.

1. #640 accepted by the owner. (Merged in #644; the issue stays open until
   accepted.)
2. #639 accepted by the owner: the held-plan and unobserved-controller states
   verified and the operator workflow accepted. #641 and #643 are merged.
3. A **human-operated** Product Workspace preflight by the operator against the
   intended intake path, with setup friction logged honestly rather than
   concealed. Automated Chrome evidence does not substitute for it.
4. Post-merge `go` on the exact frozen base SHA (the merge commit) is green.
5. The freeze record of section 2.3 exists, and `main` has not moved since.
6. The base, budgets and this file digest are recorded in the launch request.
7. The controller of section 2 is established through the supported lifecycle
   (demonstrated end to end on a fresh state directory): operator
   configuration with `github.governance_credential_mode` `github-cli` and a
   signed-in `gh`, a running Docker daemon with the configured pinned assurance
   image, then `controller build-adopted --repo <owner/name> --config <path>`,
   then, with `serve` stopped, `controller re-adopt --reason <text>` run from
   the published `main-<sha>/zenchron-engineering` artifact itself (not a copy),
   then `serve` and `control-plane` from that artifact. Set
   `ZENCHRON_CONTROLLER_ROOT` identically for every one of these commands when
   an isolated controller root is used. No activation record is hand-written.

## 4. Decisions recorded from the owner

- Decided 2026-10-19: 3600 s assurance, 1800 s execution, the 8/2/20 limits,
  28800 s aggregate, the file name, and that the benchmark base is the merge
  commit of this file (section 2).
- The invalid test run stays recorded as overhead and finding evidence, outside
  both arms.
- **Trade-off, stated plainly:** exact base equality requires `main` to stay
  unchanged throughout both measured arms. If development must keep merging into
  `main`, a governed pinned-base capability in the planner has to exist before
  launch instead; a docs-only descendant is not accepted.

## 5. What this document claims

No leverage ratio. No measured result. No acceptance of #639, #640, #481, #479,
#66 or #471. The operator log from the test run exists and is preserved
unedited. Nothing here is a launch authorization.
