# Local Runtime v0.1

Runtime objects (`EngineeringRun`, `EngineeringEvent`, `RunOperation`, and
`RunSnapshot`) are versioned JSON objects with `schema_version: "0.1"`.
`phase` is an operator projection (`contract`, `execute`, `observe`, `assure`,
`authorize`, `remediate`, `publish`); disposition is independently `active`,
`waiting`, `completed`, `failed`, or `cancelled`.

Events are append-only and use the finite catalogue defined in `runtime`.
`operation.planned`, `operation.before`, and `operation.after` are the only
operation lifecycle points. Hash inputs follow this exact path: typed runtime
object -> `encoding/json` valid JSON -> RFC 8785 JCS `Transform` -> SHA-256.
`encoding/json` is not canonicalization. Invalid UTF-8 strings, non-finite
numbers, and integers outside the I-JSON interoperable range fail before
hashing; invalid raw JSON fails before transformation and duplicate-name raw
JSON fails during JCS transformation.
State hashes are SHA-256 of canonical reducer state with `state_sha256`, the
journal cursor, and its hash-chain fields excluded. Event hashes are SHA-256 of
the canonical event with only `event_hash` excluded, retaining its
state-before/state-after and chain bindings. The reducer never reads wall time.

Every persisted event carries its `event_hash`, in its document and in its
indexed column. A persisted event whose `event_hash` is empty is corruption, on
the run stream and the plan stream alike, wherever it sits in the chain and
whether or not anything else about it changed: the shared read that every
replay and event read passes through refuses it, and the reducers have no
empty-hash tolerance of their own (#462). The only event that is ever hashed
without a stored hash is the one being appended: inside its append
transaction, before `state_after` is known, it carries a provisional hash
(its digest with `state_after` still empty) so the reducer can fold it, and
the final hash replaces that before the row is written. The state digest
excludes the cursor, so the provisional value never reaches it.

An event's `state_before` and `state_after` are recorded transition digests:
diagnostic compatibility metadata, not replay-verification inputs and not an
integrity or authority anchor (#453). Replay authority is the event's type,
stream and sequence, agreement between its indexed columns and its document,
its link to the previous event, its `event_hash`, the validity of its
artifacts, and the deterministic fold over its events. Replay does not
recompute or compare the recorded digests, and no cache, checkpoint, recovery
path, admission, authority or other runtime decision may trust either field
in place of replay. Because `event_hash` covers them, changing either field
without recomputing that hash is corruption and is refused like any other
tampered event, including when the hash is blanked rather than left stale; a value that is arbitrary but chain-consistent is accepted
and still decides nothing. The physical JSON names stay for journal
compatibility. A later journal-format revision may remove the fields, but
historical events are never rewritten merely to rename them.

Runtime data is stored in the operator state directory, not the target repo.
Artifacts are references only and distinguish raw/local-only data from
sanitized/publishable data.  The #30 extension and #31 checkpoint shapes are
reserved but neither executes repository commands nor imports state in M0.

## Local scheduling and recovery

`RunOperation` uses a stable idempotency key scoped to its run and kind, held
as a unique durable constraint. Operations are persisted in SQLite, in
`runtime.db` under the operator state directory; an in-process store is a test
double only. A process-local mutex over a read-modify-write JSON document is
not a durable store: it protects a single process and therefore provides no
atomic lease or cross-process idempotency guarantee.

The database carries an explicit schema version applied through ordered
migrations, and the runtime refuses to open a database whose schema version is
newer than the binary supports. Each operation is a durable canonical-JSON
`RunOperation` document. Creation, lease acquisition, heartbeat, state
transition, and lease clearing are transactional; attempt and budget updates
are atomic; row revisions are updated by compare-and-swap so two scheduler
processes cannot both believe they acquired the same operation. Queue ordering
is deterministic and survives restart, and crash/restart recovery is a store
obligation rather than a caller convention.

The same operational state also holds the persistent run identity and the
append-only event journal, under the same schema-version and ordered-migration
mechanism. Sequence numbers are allocated monotonically per run, an append is
transactional, and a duplicate or otherwise invalid sequence is refused rather
than reordered or overwritten. The journal's hash chain is persisted and
verified, and deterministic replay rebuilds the run snapshot after the
database is reopened. Canonical event rows hold references only; raw
transcripts never live in them.

Event identity is durable and caller-owned: the event id is the journal
table's primary key, while the sequence, the chain links, and the
state-before/state-after digests are allocated inside the append transaction
and may never be supplied by a caller. Two independent journal writers over
the same database - a watch controller holding the run's operation lease and
an operator command holding none - therefore append concurrently without
colliding and without a gap, and neither derives identity from an in-process
counter or from the clock. An operator-recorded fact is appended straight
through the journal and takes no operation lease, because recording that a
human answered is not an engineering side effect and must not have to take a
lease a controller may be holding.

The runtime plans operations deterministically and defaults to one concurrently
driven local run. Operation leases bind a runtime-instance owner, heartbeat,
and expiry. Owner identity binds host, process ID, and process-start identity;
expiry alone is insufficient to take over when the owner remains live. Time is
injected into scheduler/budget code and is never read by the reducer. A crash
after `operation.before` is reconciled through the operation's idempotent
side-effect probe; an unprovable result is `unknown`, not a retry.
Cancellation is a common process-control request shared by future provider and
assurance adapters. Run/operation wall limits, attempts, and no-progress keys
are persisted operational state.

Owner liveness is evidenced by an OS advisory controller-instance lock held for a
runtime instance's entire lifetime under the state directory and released by
the kernel when that process dies, which is the only evidence that survives a
hard crash. A lock FILE is never liveness evidence: its presence proves
nothing, and a platform that cannot decide whether the lock is held reports
the owner alive, so takeover is blocked rather than guessed. Liveness is
one-sided by construction - an owner is alive unless it can be positively
proven gone - so an unparseable owner identity, another host, or an
unreadable process all refuse takeover rather than allow it.

Nested verification (#496) shares this scheduler and database. A canonical-JSON
`VerificationPermit` binds the parent run, operation, physical attempt, controller
owner, tool execution identity and ownership lock, with `waiting`, `granted` and
`released` states. Its row and timestamped `verification.permit_changed` event
transition atomically. The shared ceiling counts assurance leases and granted
tool permits; a nested grant consumes no additional work slot or attempt.
Only durable admission permits execution. Capacity wait is excluded from run,
plan and operation active-work accounting; tool execution is charged. The existing
absolute attempt deadline remains fixed; capacity waiting does not extend it.

Native adapters wrap exactly their existing contract-required executable grants
with attempt-owned PATH shims. Requests pass through the existing scratch grant;
the controller performs store transactions and signs replies with an attempt key
whose public half is in the runtime-owned descriptor. No database write permission,
provider hook, network permission or executable permission is added. `candidate.run`
uses the same permit owner before entering the existing sandbox. Tool descendants
retain the ownership fd; recovery requires death plus expiry, and container grants
also require exact-container reconciliation. Unknown cleanup retains occupancy.
Native `operator_trusted` workers can deliberately bypass PATH or alter their own
execution environment; this mechanism never establishes protected isolation.

## Operator pause

Status: **frozen** (#86, maintainer decision of 2026-10-03, including both
narrowings below). This section is that design as committed spec.

```text
autonomy pause RUN --reason T   ->  journal: run.paused   {reason, operator}
autonomy unpause RUN            ->  journal: run.unpaused {operator}
paused(run) := the latest of {run.paused, run.unpaused} in the run's journal is run.paused
```

The names are `pause` and `unpause` because `hold` (#203 held material) and
`resume` (`autonomy resume`) are taken. A pause is **a fact in the journal and
nothing else**: no column, no mirror on the run row, no in-memory flag. It is an
**orthogonal** fact: it is never a `Disposition` and never a reusable `Reason`,
and it changes neither.

It is enforced in exactly two places, both reading that fact:

1. **`AcquireOperation` is the gate.** One more conjunct in the same atomic
   `UPDATE` that grants a lease, beside the terminal-run check, reads the run's
   latest pause event (backed by the partial index `events_run_pause`). A
   paused run is granted no lease of either capacity class: no work and no
   observation. A driver that read the run before the pause is refused there.
2. **`Reconcile` exits early** right after loading a paused run, beside the
   `driven_elsewhere` exit, returning the run's own disposition and reason. It
   plans, settles and journals nothing. It is an optimization and the
   no-journal guarantee, never the gate. A pass whose acquisition the store
   refused because a pause landed mid-pass likewise leaves the run unsettled.

**In-flight work is not interrupted.** An operation leased before the pause
finishes under its own existing bounds and its result is journalled; the next
acquisition is refused. Interrupting a provider on pause is out of scope
(`stop RUN` is terminal, `shutdown` is fleet-wide and resumable).

Invariants:

1. Durable and restart-safe: replay, the acquire predicate and status all read
   the journal events, so a restarted `serve`, another process, or a reopened
   store sees the same answer.
2. Never cancels or terminalizes: the pause causes no
   `run.waiting/failed/cancelled/completed`, finishes no operation and
   requests no cancellation. A pass that already decided before a pause
   committed mid-pass may still journal those decisions (`operation.planned`,
   a `run.waiting` or `run.failed` it derived earlier); they are ordered
   before the pause took effect and are not caused by it.
3. Never discards material: only the pause event is written; GC already keeps
   a nonterminal run's material.
4. The store decides atomically (above).
5. Idempotent: `pause` on a paused run appends nothing and reports the
   original `since`, `reason` and `operator`; `unpause` on an unpaused run
   appends nothing; the decision is made inside the append transaction, so two
   racing commands append one event. Both refuse a terminal run and append
   nothing. Nothing but `unpause` clears a pause: not a forge event, a config
   change, a plan tick, a restart or `autonomy resume`, which refuses a paused
   run with the `unpause` hint. `stop` on a paused run cancels it; cancel
   dominates.
6. Authority is the operator's owner-only access to the state directory. With
   a supervisor running the command is delegated through the control endpoint
   and applied under the controller role (`WithAuthority`), so a superseded
   generation cannot accept it; an endpoint that exists but cannot be reached
   is a refusal, never a local write beside it. With no supervisor it is
   written locally. This is plan approve's routing; `stop RUN` writes locally
   even beside a running supervisor. The recorded `operator` is the
   requester's resolved identity: provenance, never authority.
7. No authority or evidence, and no change to trust, budgets or ceilings. The
   reason is bounded payload text and never part of an event id
   (`<run>-paused-<unixnano>`).
8. No budget movement: paused time is non-active because nothing runs;
   `lifecycle_deadline_seconds` keeps running because it is calendar time.
9. Unknown is never "not paused": if the predicate cannot be evaluated the
   acquisition errors; a run whose journal cannot be replayed is
   `unavailable` in status; a command that cannot read the journal refuses.

`RunSnapshot` gains `paused`, omitted when empty, so every existing run replays
to the same state digest. Status shows `paused {since, reason, operator}` per
run and a fleet **paused** count (a paused run holding no active operation),
taken out of runnable/waiting, so
`working + observing + runnable + waiting + paused + unavailable == active`. A
paused run still settling counts as working or observing. The verification
counts (#490) refine this partition rather than join it: `verifying` is the
working runs holding a verification slot and `awaiting_verification` the
runnable runs whose next operation is a verification, beside
`verification_capacity`.

Upgrade and rollback. The binary that introduces pause adds one migration,
the partial index `events_run_pause`, applied with the schema version bump in
one transaction (seconds on a large store). Mixed versions fail closed: an
older binary refuses to open the newer schema version, exactly as for every
earlier migration. A downgrade therefore needs `DROP INDEX events_run_pause`
and decrementing `PRAGMA user_version` by one, done by hand with every
controller stopped. Even then, a run that was EVER paused cannot be replayed
by an older binary, which does not know `run.paused`/`run.unpaused` and
refuses the unknown event type; such runs stay unreadable to it until the
newer binary is restored.

Out of scope: provider interruption on pause, observation while paused (forge
movement is observed on the first pass after `unpause`), scheduled or automatic
pause, fleet-wide pause (`drain`), and any change to `stop`, `drain`,
`shutdown`, held material or opt-out semantics.

## Configuration layers

Configuration is two layers with strictly different authority. The operator
(global) layer lives outside every repository the runtime works on and is the
only place that may authorize a credential, name the AI provider and the path
to its credential, name the assurance sandbox image, choose the state
directory, set run budgets, enrol repositories for watch, choose the opt-in
label, set the reclamation window, or name the operator identity a run is
recorded against. The repository layer is an in-repo file read from the
controller checkout root, and it may only TIGHTEN: every member it may name
is a bound it can lower and never raise. A repository the runtime is changing
therefore cannot rewrite the terms under which it is changed.

Nothing in the repository layer can raise a capability, a concurrency
ceiling, a budget, credential authority, or publication authority. The
members it may name are a stated allowlist enforced before decoding rather
than an emergent property of a struct, so adding a field to the repository
configuration type cannot by itself hand a repository a new authority. It may
lower a run budget, lower the concurrent-run ceiling, and ask to be polled
less often; it may name no credential, provider, endpoint, transport,
assurance image, state directory, retention window, watch enrolment, opt-in
label, or operator identity. A proposal that would raise a bound is refused
rather than clamped, so a repository cannot probe the ceiling and be silently
overruled. Both layers decode strictly - exactly one JSON value, no duplicate
members, no unknown members - and each digests to a stable SHA-256 over its
canonical form, so a run records exactly which configuration governed it
rather than a path that may have moved.

The physical provider-attempt wall is a last-resort hard fuse (#497), separate
from provider inactivity and cumulative run active-work authority. An unstated
`attempt_wall_limit_seconds` remains absent from canonical operator configuration
and its digest. New runs freeze `min(3h, wall_limit_seconds)`; an explicit
operator value is preserved, subject to the run ceiling. Repository tightening
against an unstated operator limit uses the same `min(3h, operator wall_limit)`
ceiling. Existing frozen policies and pre-#328 operation-remainder semantics
are retained. Each physical attempt fixes its deadline to the smaller of its
frozen fuse and remaining run active work, with that exact instant and bound
recorded in provenance. Recognized progress and known tool execution remain
supervised by the existing inactivity oracle; none can extend the physical fuse.

## Candidate repository boundary

Candidate repositories are full runtime-managed clones beneath operator state,
with their own writable Git metadata. They are never controller worktrees.
Only the runtime commits candidate files. Before it does so it canonicalizes
changed repository paths, rejects symlinks, ignored/unsafe paths, size-ceiling
violations and obvious credential/private-key content, captures commit/tree
identity, and requires a clean working tree. A producer change to refs,
remotes, config, or other protected Git metadata is a
`workspace_integrity_violation`; it is refused/restored rather than adopted.
Base drift rebases before publication; after publication it is integrated by a
merge-from-base and never a runtime force-push. Conflicts are typed outcomes.

The runtime commit carries exactly the tree its gates judged. It writes the
staged index to a tree once (`write-tree`), takes the blob list from
`diff-tree -r --raw --no-renames <parent> <tree>`, runs the commit gates and
the raw-worktree backstop over those blobs, then builds the commit with
`commit-tree <tree> -p <parent>` and moves HEAD with
`update-ref --no-deref HEAD <commit> <parent>`. It never reads the index again,
so an index rewritten after gating cannot change what is committed. A HEAD that
moved from the gated parent is refused as a `workspace_integrity_violation` and
no commit is reported. Every runtime Git call ignores replace objects
(`GIT_NO_REPLACE_OBJECTS=1`), so a `refs/replace/*` ref cannot make the gates
read stand-in bytes for a blob the commit carries. Every runtime Git call,
and every Git command the broker runs for a provider, also reads grafts from
the null device (`GIT_GRAFT_FILE=/dev/null`), so `.git/info/grafts` cannot
rewrite a commit's parents. The commit's recorded paths
are that tree diff, the same set #431 recovery recomputes.

After the runtime writes the commit object, the provider-writable candidate
repository is no longer a content authority (#437). The runtime fetches the
exact commit into a runtime-owned subject store beside the candidate workspace
(`runs/<id>/subject.git`), never inside it. Every object the fetch receives is
re-hashed by `index-pack`, so content that does not match its name is refused
rather than stored. This covers a loose object swapped behind its name and a
name answered from `objects/info/alternates`.

Once a runtime commit is made, no production decision, reviewer context, recovery check, assurance checkout or publication path may obtain committed content/ancestry from the provider-writable candidate object database.

These readers all read the subject store, and none of them reads the
candidate's worktree, index or object directory:

- the commit gates;
- observation and contract reassessment;
- the assurance and semantic checkouts;
- #431 recovery;
- `candidate.push`, and its diagnosis of whether the remote branch head is an
  ancestor of the candidate;
- the check of whether an observed pull-request head is the runtime's own;
- the plan reconciler's proof that one upstream candidate contains another;
- the upstream diff a reviewer is given;
- the transfer of an upstream candidate into a downstream run.

A head or a sibling candidate that is not in the store's history is never
related to the candidate. A foreign pull-request or remote-branch head is
therefore recorded as `candidate.external_changed`. A commit whose content
cannot be fetched under its own names fails closed and never falls back to the
candidate: before HEAD moves, nothing is committed; after it, the failure is a
#402 post-commit failure. Assurance checkouts are cloned from the store
without hardlinks. Committed `.gitattributes` apply in them as repository
semantics, and the candidate's `.git/info/attributes`, replace refs, alternates
and index state do not. The post-commit metadata baseline is derived: it is
the config and refs the commit's integrity check approved, with HEAD at the
new commit. It is never re-read from the live repository. A live digest that
differs from it is a `workspace_integrity_violation`, reported with the
commit. The baseline re-reads after a rebase or a merge from base are #466.

The trusted Git metadata baseline is persisted rather than re-derived. Every
runtime-owned Git operation that succeeds journals the digest of the
runtime-owned Git metadata it established, and integrity is asserted against
that journalled digest rather than against whatever the live repository
currently says. The check is therefore a cross-process guarantee: a restart
cannot launder metadata that was tampered with while no runtime was running,
because the baseline is not the workspace's own opinion of itself. A
journalled candidate head move clears the baseline and the operation that
moved the head re-establishes it from its own `operation.after`, so a
baseline stays bound to the head it was taken at; an operation interrupted
before it recorded one leaves no durable baseline and is seeded rather than
believed against a head the workspace has since left.

## Kernel bridge

The runtime has one deterministic bridge for candidate mutations:

```text
source snapshot -> ProjectModel/facts -> WorkContract -> runtime commit/tree
-> normalized observed change -> reassessment -> evidence lifecycle -> authority
```

Reassessment remains the #8 evaluator and authority remains the #7 evaluator.
Material scope changes refresh the exact subject binding and stale prior exact
evidence. A requested privilege expansion and `awaiting_authority` are durable
waiting states, never remediation routes.

## Provider and verifier boundaries

`ExecutionProvider` and `AssuranceProvider` are generic runtime adapters, not
authority-bearing domain types. An execution request binds the run, pinned
source snapshot, controller, base revision, candidate commit/tree/workspace,
current contract, objective, obligations, constraints, prohibitions,
permissions, trusted controller instructions, purpose, remediation findings,
and budgets. Provider output is an observation only and is never acceptance
authority.

Provider inference connectivity is not candidate command connectivity:

```text
provider inference connectivity/auth  !=  candidate command connectivity/credentials
```

Three capabilities are bounded separately. The provider control plane may
authenticate explicitly to its configured AI provider and may hold the
connectivity that remote inference inherently requires; it receives no GitHub,
SSH, signing, cloud, or application credentials, and its result carries no
acceptance authority. Candidate and tool execution is confined to the bounded
candidate workspace: no ambient host environment, none of those credentials,
network denied by default, and no reach into the runtime database, the
controller checkout, or another run. Assurance is bounded separately and more
strictly still, as described below.

The M0 Codex adapter is a native compound provider driving the installed Codex
CLI. A Codex control process with no network and no authentication cannot
perform remote inference, so an isolation model that demands one is not
implementable. It does not, however, satisfy the boundary above. Local Codex
`--sandbox workspace-write` with `sandbox_workspace_write.network_access=false`
primarily bounds writes and does not establish bounded reads: provider tool
execution under that mode is not proven unable to read the runtime database
and runtime state, runtime locks, the controller checkout, another run's
state, provider credentials, or unrelated home data. Filesystem read isolation
for the native Codex adapter is unproven, and the required filesystem
confidentiality is not weakened to make the adapter appear compliant. The
adapter is retained explicitly as a bootstrap, operator-trusted adapter, and
it is ineligible for protected autonomous execution. Eligibility fails closed:
a provider that does not prove the required isolation properties, or does not
report them at all, is refused rather than assumed compliant.

What the adapter does prove is stated without overstatement. Capability
probing is a fail-closed precondition of execution, and the adapter never
silently uses full-access or unsandboxed Codex. Runtime-owned policy defines
the effective sandbox constraints; arbitrary user Codex configuration does
not. The candidate command environment is an allowlist built from scratch
rather than an inherited ambient environment. Provider authentication is a
control-plane capability and is never copied into a candidate command
environment. A candidate `AGENTS.md` is left unmodified and cannot become
trusted instructions.

The protected provider separates reasoning from tool execution:

```text
reasoning provider
    -> tool request
    -> Zenchron-controlled ToolBroker
    -> candidate operations
```

Zenchron validates and executes each requested capability; the reasoning
provider never touches the filesystem directly. The initial capabilities are
repository read, repository search, candidate diff, apply patch, and run
bounded command. Candidate command execution continues through the existing
sandbox and receives the candidate workspace only: no runtime state, no
controller checkout, no other run's state, no provider credentials, and
network disabled by default. Provider credentials remain solely in the
control-plane process, and `ExecutionProvider` stays provider-independent.

That brokered adapter exists. The OpenAI Responses provider runs its
reasoning loop in the trusted controller process; the model emits only tool
names and JSON arguments, and there is no hosted shell, no code interpreter,
no file-upload channel, and no path handle crossing the boundary. Its API key
is an operator-supplied file reference, read inside the call, held in a local
variable, used only as a control-plane request header, and never placed in an
execution request or result, in the broker, in a brokered command
environment, or in a transcript; a key file resolving inside the candidate
workspace is refused, so repository content can neither supply nor redirect
the credential. The broker is bound to the candidate workspace named by each
request and a broker bound to any other tree is refused. This is the provider
that reports all four required isolation properties as proven and is
therefore the protected-eligible one. Eligibility remains a fail-closed
consequence of what a provider reports rather than of which adapter it is:
a provider that reports any required property as unproven, or reports
nothing at all, is refused. Provider completion remains an observation and
asserts nothing about acceptance.

The trusted instruction view is supplied out of tree and separately from the
candidate workspace, so candidate instructions cannot become trusted in the
same run. Candidate Git metadata and remotes are not an adapter capability; a
producer that reaches them raises the `workspace_integrity_violation` of the
candidate repository boundary and is refused or restored rather than adopted.

`DockerSandbox` remains a valid isolation primitive for assurance and for
providers whose control and tool architecture supports it. Providers are not
required to be Docker-shaped, and `ExecutionProvider` stays
provider-independent. Where a bounded workload is Docker-contained,
unavailable Docker isolation is a refusal, never an unsandboxed fallback.

Every assurance invocation receives a fresh detached checkout of the recorded
candidate commit/tree, not the producer workspace. The baseline Go verifier
uses a read-only checkout, no network, isolated HOME/temp, masked Git metadata,
and no runtime/controller/other-workspace mounts. Dependency preparation is a
separate constrained `go mod download` step using only an operator-provisioned
pre-warmed cache. The exact checkout stays read-only and `-mod=readonly`
prevents any proposed `go.mod` or `go.sum` rewrite; incomplete module metadata
therefore returns a typed dependency-preparation failure rather than mutating
the recorded candidate. The resulting cache is mounted read-only for offline
`gofmt`, `go vet ./...`, and `go test ./...`, which retain `-mod=readonly` and
network-off settings. A future networked preparation policy, if approved, must
operate on a disposable copy and route any module-file change through a new
candidate commit, reassessment, and assurance.
The verifier records its definition digest and confirms the exact tree after
verification.

Provider and verifier stdout/stderr are raw local-only artifacts. A separately
stored sanitized candidate is still non-publishable until an explicit
publication decision; transcript bodies are never embedded in run/event rows.
Docker daemon connectivity is explicit trusted controller configuration through
`DockerEndpoint`; it is passed to every runtime Docker CLI operation as a
control-plane argument and is never inherited from ambient `DOCKER_HOST` or
forwarded into a candidate, provider, or verifier container. The endpoint and
the daemon's reported ID are part of the operation-record and container-name
binding; reconciliation reprobes that ID and fails closed rather than targeting
a record created for another configured or replaced daemon.
Every Docker invocation has an exact runtime-owned operation record outside
the candidate, written before `docker create`, and a generated exact container
name. Cancellation and crash reconciliation target only that recorded name:
TERM, bounded inspect, KILL when needed, `wait`, remove, and final inspect.
Absent records mean no container is targeted; an already absent or exited exact
container is recorded removed; daemon/inspect uncertainty is an explicit
ambiguous outcome, never a broad cleanup attempt.
The local Docker CLI is also cleaned up as a host process, but is never proof
that a Docker workload stopped. No candidate container receives the Docker
socket. Cancellation owns the subprocess group: graceful stop first, then bounded
force-kill, with a conservative terminal operation result. On Linux, the
runtime also records descendants of its direct child from `/proc` and binds
each recorded PID to its process start time before signalling it. This keeps a
descendant that invokes `setsid` inside the runtime-owned execution from
escaping the original process group, while avoiding signals to recycled or
unrelated host PIDs. The tracker is joined before the invocation returns.
Other Unix hosts retain process-group containment; detached-session host
execution is therefore not an M0-supported containment mode there. M0 verifier
work is Docker-contained. A bootstrap native provider is contained by the
sandbox mode it proved with its own runtime plus this runtime-owned host
process containment; that containment bounds process lifetime and writes, not
reads, and is not protected-execution eligibility. Windows refuses the bounded
adapter until Job Object containment is implemented.

## Failure routing

Failure routing is typed and bounded. Format failure invokes deterministic
`gofmt`; compile/test failure may invoke a bounded execution remediation;
transient provider/infrastructure failures consume retry budget; material scope
or verification-surface changes re-enter reassessment; authority waits never
route to a producer; integrity violations restore/refuse; unknown failures
stop diagnostically. Every mutation, including `gofmt`, is committed by the
runtime and flows through normalized observation, #8 reassessment, a current
contract, and fresh exact-tree assurance.

### Retry dispositions

An identical deterministic failure with no intervening relevant state change
is not a retry candidate (#190). The shared operation gate checks this before
`StartWithin`, so withholding an attempt spends neither attempt budget nor a
physical attempt identity. It applies to every operation kind, including an
operation the scheduler selected while the planner wanted a different one.

Known local checks and the pure policy compiler supply typed failure causes.
The runtime records an optional `failure` object on `operation.after`:
`classification: deterministic_local`, a runtime-authored `code`, a canonical
SHA-256 `signature` of that check, its typed `check_path_base64` when it has a file
subject, and the operation binding, plus `binding_sha256`
over relevant durable inputs. No diagnostic prose is parsed or hashed to make
this decision, and no provider can supply the object. An error from an analyzer,
subprocess or filesystem is not guessed deterministic merely because it repeats.
The file subject uses Go's JSON byte-slice encoding (base64), preserving
exact filesystem bytes instead of letting JSON replace a non-UTF-8 filename.

Input bindings are check-specific. Sharing an operation does not make its
source, policy, model or controller configuration inputs to a candidate-only
check. Only state capable of changing the owning check renews eligibility:

| Check | Relevant durable input |
| --- | --- |
| `policy.compile` | Compiler version, pinned source, governing contract, ProjectModel, policy and committed subject. No index or uncommitted scratch. |
| `candidate.sensitive_path` | Whether the captured refused path would still be carried by staging. Changing its bytes or unrelated policy cannot fix its name. |
| `candidate.credential_file`, `candidate.symlink` | The captured path's existence and file kind/name predicate. |
| `candidate.index_flags` | Flagged index entries, without blob bytes or index timestamps. |
| `candidate.residue` | Dirty candidate paths outside the recorded commit's exclusions. Changing residue bytes without removing its dirty path does not fix cleanliness. |
| `candidate.metadata_integrity` | Expected revision/metadata, recorded recovery proof and observed Git metadata; no producing-operation/checkpoint labels. Failed recovery I/O is not a local integrity verdict. |
| `candidate.visible_credential_value` | The captured candidate-visible file's content/existence/kind; no index or policy. Directory links are not followed. |
| `candidate.staged_credential_value`, `candidate.worktree_divergence` | The captured file's bytes, index blob entries, baseline entry and resolved conversion attributes/configuration. No unrelated file content or index mode bits. |
| `candidate.size_limit`, `candidate.empty_tree_diff` | Eligible candidate content, index, base tree and resolved conversions; size ceiling for size, executable/kind modes for tree difference. |
| `candidate.empty`, `candidate.path_shape`, `candidate.scratch_only`, `candidate.exclusion_record_limit` | Changed paths and the structural eligibility/exclusion decision, as consumed by the owning check. |
| `format.go_paths_missing`, `contract.semantic_claim_missing`, `kernel.source_unpinned`, `operation.handler_missing` | Respectively: Go paths, required semantic claims, source presence, installed handler implementation identity. |

Index timestamps, attempt counters, journal cursor, leases, waits and time are
excluded. Binding happens after the failed operation's partial effects, so a
commit already made cannot count as an intervening fix on the next reconciliation.

An unchanged binding settles `waiting/deterministic_failure_unchanged`. Candidate
material and recorded runtime commits stay preserved; no assurance, publication
or acceptance is granted by the wait. Relevant state change permits another
attempt under the original ceilings and ordinary integrity/authority checks.
The wait excludes idle time from active-work accounting, including downtime
after the journalled failure and before settling the wait; the existing lifecycle
deadline still applies. The failed operation owns that interval. Its next
`operation.before` closes the deterministic wait at actual retry resumption;
other operations' probes only add their work, without closing it. Retry time and
elapsed time after recovery count normally, including after restart. A new
deterministic failure opens a new interval. Transient classes and provider
correction routes retain their existing semantics. Explicit `unknown` still stops; an unclassified error
keeps its legacy budget-bounded behavior.

Failure to observe inputs records `binding_error` and an empty input digest,
persists the original outcome (including a commit already produced), and returns
the observation error. Missing identity never proves equality. A later comparison
also reports unreadable inputs without starting another attempt; once readable,
an unbound prior outcome cannot prove unchanged state.
An older draft failure missing the captured subject needed by a scoped check is
also unresolved: it cannot manufacture another attempt by guessing the subject.
`binding_scope: check-input-v1` identifies the scoped binding definition. An
older draft's broad, unversioned digest or an unsupported scope cannot be
compared as if a hash-format change were a relevant input change. Its durable
outcome remains readable, but comparison reports unavailable identity without
starting another attempt. No journal is rewritten or backfilled.

Compatibility: the field is optional, so historical records have no inferred or
backfilled classification. It uses the existing operation JSON and journal; no
SQLite migration, budget reset or history rewrite occurs. New readers accept old
journals. Older strict readers can reject new lifecycle payloads containing
`failure`; downgrade of a store written by this version requires a compatible
reader, not stripping the field or rewriting the journal. Mixed controllers
remain subject to the existing identity, lease and succession gates.

`RouteFailure` is the only routing table: every failure class has exactly one
route. Some classes also carry a typed **retry disposition**, persisted on the
operation as `retry_disposition`, which owns how a retry is accounted. The
operation's `retry_not_before` is purely temporal ("not eligible before T"). It
carries no meaning of its own: the waiting reason, any attempt refund, whether
the wait counts as external, and whether an observation keeps its binding are
all read from the disposition, and never from whether a timestamp is set.

| Disposition | Produced by | Spends attempt | Spends active work¹ | Finite attempt authority | Resume condition | Waiting reason |
|---|---|---|---|---|---|---|
| `transport_backoff` | `connectivity_unavailable` | yes | no | yes | `retry_not_before` has passed (30 s, doubling, capped at 5 min) | `connectivity_backoff` |
| `provider_availability_wait` | `provider_unavailable` | no | no | no | the provider endpoint recovers on its own; probed every 5 min | the class's: `execution_provider_unavailable` |
| `rate_limit_wait` | `provider_quota`, `provider_rate_limited` | no | no | no | the provider allowance returns; probed every 5 min | the class's: `execution_provider_quota` / `execution_provider_rate_limited` |
| `account_wait` | `provider_account_unavailable` | no | no | no | an operator restores the provider account; probed every 5 min | the class's: `execution_provider_account_unavailable` |

¹ Waiting only. A probe's own execution time is refunded only when
`provider_executed` is false (see Provider waits).

Further rules:

- **Finite attempt authority.** A `transport_backoff` retry draws on the
  operation's existing `max_attempts`. The final attempt gets no backoff and no
  `connectivity_backoff` wait. It stops as `<operation>_attempts_exhausted`.
- **Transport cause.** The cause is classified once, from typed errors only.
  Every forge adapter (REST, GitHub App token exchange, governance) routes its
  failed exchange through that one classification and keeps the cause on a
  typed transport error.
  - Only transport loss is `connectivity_unavailable`: a temporary or timed-out
    typed DNS error, `ETIMEDOUT`, an unreachable host or network, a reset, or
    a refused connection. These are the only timeouts that count as loss. An
    `http.Client.Timeout` surfaces as a context deadline and is classified
    `caller_cancelled`.
  - These fail closed rather than backing off: DNS not-found, TLS or
    certificate failures, proxy configuration, and the caller's own
    cancellation or deadline.
  - **Explicit exception: provider CLI text.** A provider CLI has no typed
    errors to read, so it reaches `connectivity_unavailable` by naming transport
    loss in its own terminal diagnostic, matched against a closed signal list.
    That list includes NXDOMAIN-like text such as Node's
    `getaddrinfo ENOTFOUND`, Codex's `dns error`, and Claude Code's `is_error`
    result whose whole text is the CLI's "Can't reach the API server … (ENOTFOUND)"
    envelope. For a CLI that text is how it reports losing the network, so a
    typed DNS not-found failing closed does not extend to it. Text is compared
    in lower case, with U+2019 read as an ASCII apostrophe.
- **Endpoint availability.** An overloaded endpoint or a 502, 503 or 504 is
  `provider_unavailable`, routed `wait` under `provider_availability_wait`.
  The failure class stays the exact cause; the disposition names only how
  the retry is accounted.
- **Class to disposition (frozen, #87).** `provider_unavailable` →
  `provider_availability_wait`; `provider_quota` and `provider_rate_limited`
  → `rate_limit_wait`; `provider_account_unavailable` → `account_wait`;
  `connectivity_unavailable` → `transport_backoff`. No other class carries a
  disposition, and every disposition has a producer: these four are the
  whole vocabulary. A concrete executable or prerequisite disposition, with
  its own rules, is #87 slice 3's to introduce after #84.
- **Provider waits (#87).** The four provider classes keep `RouteWait`. Their
  disposition is external wait and sets `retry_not_before` to a fixed 5
  minutes after each probe: an attempt-keyed backoff would never grow,
  because the attempt is given back. The probe time is on the operation row
  and in its journalled `operation.after`, so a restart before it invokes
  nothing. Once it passes, the same operation of the same run is invoked
  again.
- **Provider wait accounting (frozen, #87).** A recognized provider wait
  always refunds the logical engineering attempt. The attempt's active
  execution time is refunded only when `provider_executed` is false, that is
  when the provider was refused before it ran. The provider owns that fact,
  never the failure class: a CLI reports it with its process provenance, and
  `openai_responses` once one model exchange has completed, so a tool loop
  that ends in a 429 keeps its elapsed work charged. When the provider really ran,
  that elapsed work stays charged to the active-work budget. There is no
  "made no progress" heuristic: a long wait therefore spends active work one
  probe per 5 min and is bounded by the active-work budget as well as
  `lifecycle_deadline`.
- **No early probe (frozen, #87).** Restoring the provider account, or any
  other external fix, does not bypass `retry_not_before` in this slice.
  `autonomy resume` is an ordinary reconcile and honours it; it is not a
  hidden `--now`, and no override verb exists. The provider rows state no
  reason of their own: the run waits under the class's stated reason (quota
  and rate limit share a disposition but not an operator action). Status
  reports the next probe time and the disposition's resume condition only
  for the latest operation of its kind whose reason is the run's.
  `retry_not_before` is a floor: the supervisor gives a parked run a turn at
  most every 4 poll intervals, so the effective cadence is 5 min plus up to
  that quiet-tier re-poll delay.

Effective patience with the defaults: an execution (`max_execution_attempts`
2) waits once, for 30 s, before stopping. An observation (3 attempts) waits
30 s and then 60 s. The 5 min cap is reached only by the fifth backoff, so an
operation needs at least six attempts to wait that long.

The first failing assurance result that is a **candidate verdict** gets
exactly one identical rerun before any mutation. A candidate verdict is a pass,
or a failure the verifier judged: `format`, `compile_or_test`,
`verification_failure`, or an unpassed result naming no class (read as
verification). `CandidateVerdict` is the one definition. An infrastructure,
cancellation or prerequisite result is not a verdict: it is never rerun as a
confirmation and never half of a flake, and on either run it routes by its own
class.

Only two candidate verdicts that disagree are `flaky_verification`, and a flaky
result is never passing evidence. The rerun's verdict is not passed even when
the confirmation run passed, so no evidence bundle is bound, no claim is
satisfied by it, and it cannot reach authority or publication.
`flaky_verification` routes `stop`, and the run settles
`assurance.go_failure_not_retryable`. A result returned together with a
provider error is never passed either.

A provider therefore returns a judged failure as a result with a nil error. The
built-in Go verifier runs `gofmt`, `go vet` and `go test` under `sh -e`. Docker
records that workload's own exit status, and the verifier reads it as follows:

| Workload exit | Meaning | Class | Error |
|---|---|---|---|
| 0 | pass | none | nil |
| 1 or 2 | the candidate failed | `verification_failure` | nil |
| any other code (3 and above, Docker's 125, an unexecutable 126/127, a signal death such as 137) | not a verdict | `transient_infrastructure` | kept |
| none recorded (a start that never ran the workload, a container that could not be reaped) | not a verdict | `transient_infrastructure` | kept |

Cancellation keeps its error and its owner's class.

Known ceilings, accepted deliberately:

- **Signal deaths are infrastructure.** Docker's OOM record is not inspected.
  Candidate code can kill its own workload and force retries this way, but a
  non-verdict is never passed. It cannot create passing evidence or reach
  authority, and `max_assurance_attempts` bounds the retries.
- **In-container resource blips that exit 1 or 2 are read as verdicts.** A full
  tmpfs, the pids limit or a module missing under `GOPROXY=off` is
  indistinguishable from a candidate failure by exit status.
- **A blip followed by a pass stays `flaky_verification`**, terminal and not
  retryable. Contradictory evidence about the same exact tree is not weakened
  because the classifier has known ceilings.

Retry backoff for assurance `transient_infrastructure` is not part of this rule
(#465).

Before this rule the built-in verifier returned every judged failure as an
error, so it never reached the confirmation rerun and could not record a flaky
pass. A provider that reported failure with a nil error could, and a flaky pass
it recorded as a plain pass cannot be reconstructed: the journal kept only
the confirmation's verdict. No migration exists or is needed.

No-progress uses a deterministic fingerprint over candidate tree, contract
revision, failure signature, verifier, provider, and remediation identity
rather than transcript text.

## Operator surface

The operator surface is the `autonomy` command set: `run`, `status`,
`events`, `resume`, `refresh`, `authorize`, `stop`, `pause`, `unpause`,
`watch`, `doctor`, and `gc`. Each one builds dependencies from the two configuration layers, calls
the runtime, renders the result, and maps it to an exit status. None of them
orchestrates: when something happens is the runtime's decision, and what is
rendered is the operator surface's. Output is JSON by default; a text
projection renders the same structure and adds no field and hides no
refusal.

`autonomy authorize` records evidence, not authority. It appends exactly one
`human.authority_recorded` event bound to the exact state the operator was
shown, and the existing action-scoped authority evaluator decides what that
evidence means. No status is assigned, no authority evaluation is written,
and no phase or disposition is changed by recording. Recording an approval
and finding the decision still `blocked`, still `incomplete`, or `stale` are
ordinary outcomes and are reported as they are, because the evaluation is a
report rather than a grant.

Human approval is contextual evidence, never a standing permission. It
cannot expand a work contract's permissions, grant a privilege expansion,
bypass policy, make stale evidence current, or stand in for independent
assurance: it satisfies only outstanding required claims whose evidence class
is human approval, and a missing requirement that is a permission, a
capability, or an automated verification is deliberately not among them,
because no human answer supplies one. It cannot be given past a controller
mismatch, a pinned source that moved and has not been recompiled, a candidate
head observed outside the runtime, a run that is already terminal, or a
recorded decision for an action this boundary does not govern; each of those
gates recording as well as reading, so no approval overrides one. A recorded
workspace-integrity violation is not cleared by an approval either: the
failing result stays failing evidence, and approving records a human answer
to a different claim. A rejection is recorded as FAILED evidence rather than
as an absence, which is what makes it blocking rather than merely
unsatisfied.

An authority request is an operator-facing projection of current runtime
state plus the exact missing human-authority requirement, and never an
independent source of governance. It is not persisted and is not consulted
as a permission: it is regenerated from the journal on every read, and the
action, the decision, and the outstanding claims all come from the same
kernel evaluation the runtime's own authority operation performs. A run
whose evaluation reports no outstanding human-approval claim has no request
at all. Its identifier is a short prefix of a digest over its own canonical
form with only the observation members cleared, so the run, the action, the
repository, the controller identity and configuration digest, the pinned
source snapshot, the exact base revision, the exact candidate revision AND
tree, the contract revision, and the current decision and evidence context
are all inside the identifier. Naming the identifier is therefore naming
exact state, which is why an operator types a run and a request id and never
a candidate SHA or a contract revision. The digest is taken by clearing
members rather than by listing them, so a binding added later is inside it by
default. The run's whole state digest is deliberately excluded: it moves on
every appended event, including ones with no bearing on the subject, while
every governance-material component is already bound.

Any material change makes a request stale, and the old approval is refused
rather than retargeted. A superseded candidate revision or tree, a revised
contract, a changed controller, a different governed action, a changed
required-evidence context, a re-pinned source, and an integrated base each
produce a different identifier; an answer naming the old one is refused and
stays in the journal as a permanent fact about a subject that no longer
exists. Recorded human authority is admitted as evidence only for the
contract revision and the candidate revision it was given against, and
neither binding is ever rebound, so an approval survives no candidate move,
no base integration, and no reassessment. A superseded request is reported
by `status` rather than dropped, because the record still exists and still
binds nothing. The boundary raises one typed refusal and callers branch on
its code rather than on message text: no request, stale request, unsupported
action, controller changed, source intent changed, candidate externally
changed, run terminal, and invalid decision.

Recording is idempotent by identity. The evidence identifier is a digest of
the exact request binding, the operator identity, and the decision, and of
nothing else; the optional operator note is excluded, so re-answering with
different wording is the same answer and an annotation can never mint a
second record. That identifier is the journal event id and therefore the
database's primary key, so two operator processes racing the same
authorization compute the same id, exactly one insert wins, and the loser
adopts the existing record. A retry after a crash finds its own record
instead of being told there is nothing left to authorize.

A candidate Zenchron cannot authorize its own adoption. The
human-authorizable actions are a stated allowlist - open a pull request,
update one, and push the run-owned candidate branch - and adoption is not on
it: a merge or adoption action is refused as unsupported rather than given a
shape that looks governed. There is no `autonomy merge` and no automatic
pull-request merge anywhere in the runtime; a merged pull request is only
ever an observation, which completes a run passively. Final adoption of the
runtime's own candidate therefore remains external to the candidate runtime.

Operator identity is recorded provenance, not cryptographically verified
person identity. A record carries the identity the operator layer configured,
the local OS account, and the host, together with an explicit provenance
value; the only value this milestone emits states that none of it was
authenticated. Nothing is signed and no challenge was issued to anybody, so
no reader may treat the record as proof of who a person is, and a later
milestone able to verify a person adds a new provenance value rather than
reinterpreting the records already written. Resolution fails closed - an
empty identity never flows onward as if it were one - and an operator layer
may require a configured identity, which refuses the local account name as a
substitute. Identity is an operator-layer member and the repository layer's
allowlist does not contain it, so a repository cannot choose who is recorded
as having authorized a change to it. There is no role, no permission matrix,
and no auth provider: a local runtime that cannot verify a person has nothing
to attach one to.

An edit to the source issue stops the run rather than silently changing what
the run is working toward. `autonomy refresh RUN` is the only path that
re-reads changed source intent, and it grants nothing: no permission is
added, no privilege is expanded, and nothing is published by it. It is a
separate command rather than a flag on `resume`, which is what makes "a plain
resume never absorbs changed source intent" structural instead of
flag-dependent. It records operator refresh intent durably in the run's own
journal under a reason distinct from an operator stop, so the two stay
distinguishable forever, and that terminal boundary is what stales the prior
contract, the prior evidence, and the prior authority, all of which are bound
to a run that is now terminal. The successor is the next generation of the
same deterministic run identity for the same source issue; fetching the
source, snapshotting it, and rerunning source -> facts -> policy -> contract
are the runtime's ordinary operations rather than anything the command
performs itself. The prior journal is preserved exactly and the successor is
reachable from it by identity. Because the boundary is a generation, the
prior candidate branch and any pull request opened from it are not carried
across, and refreshing an already completed run is refused rather than
reopening settled work.

`autonomy resume RUN` means exactly one thing: ask the runtime to reconcile
this run again. It clears no wait. A missing forge credential, an
outstanding authority, a changed controller, and moved source intent are all
re-derived from durable state on the next pass, so a restored credential
simply proceeds while a still-changed controller or still-moved source keeps
waiting. Two conditions are refused instead of handed over, because
reconciling cannot re-derive them and proceeding would be a silent override:
a withdrawn opt-in, which is withdrawn consent, and a recorded
workspace-integrity violation against the current candidate. A cancelled run
is likewise not resumed, since explicit operator intent is not withdrawn by
asking again.

Stopping the watcher and stopping a run are different acts. Signalling
`autonomy watch` stops the watch controller: discovery and scheduling stop
and an in-flight operation unwinds through the cancellation semantics
providers and the sandbox already honour, but no cancellation is journalled,
the journal is left exactly as the last completed step wrote it, and every
run stays resumable. `autonomy stop RUN` is the only thing that cancels a
run. It is explicit operator intent and it is durable: the cancellation is
appended to the run's own journal and the run document settles as cancelled,
both of which survive a restart, so scheduling stops because the run is
terminal rather than because a process went away. The run's leased or running
operations have cancellation REQUESTED through the scheduler's existing
mechanism; no second cancellation mechanism exists, no lease another process
owns is written out from under it, and a second stop appends nothing and
reports the same answer.

Watch observes only repositories an operator enrolled in the global layer.
There is no discovery crawler, so a repository cannot enrol itself, and
durable watch state is keyed by the supplied registration and never
enumerated, so a repository the operator dropped cannot return. The opt-in
label on an issue is consent, and watch re-checks the label itself rather
than trusting the forge's server-side filter to be the authority; losing it
records an opt-in-removed wait and the run stops being driven, and a missing
forge credential records a forge-auth wait. Both are waits with a reason
rather than new work, and there is no separate paused disposition. Watch
holds no credential and none reaches durable watch state, which carries
observations only: a cursor, an entity tag, timings, a rate-limit reading,
and an error class. A per-repository fault is reported inside the tick report
and never ends the loop or affects another enrolled repository, while a
global configuration fault ends the process. Backoff grows and is capped at
thirty minutes, so a failing repository is still re-checked often enough that
an operator fix is noticed without a restart. Watch never turns its own
desire for progress into authority: a run waiting on a publication decision
is an actionable diagnostic and is left waiting, and a permission discovered
during execution is a privilege expansion that reassessment refuses.

`autonomy status RUN` is a projection over persisted state - the runtime's
status report, the folded journal, and the durable forge observation for the
run's repository. It is deterministic, so the same durable state always
renders the same view; nothing in it is a model-authored summary and nothing
is re-decided, and it makes no network call. It reports the exact request an
operator would answer, or the typed refusal raised while merely trying to
project one, together with every recorded authority decision by action, the
superseded requests, the run-driving lease, the metadata-integrity state, and
the forge wait an operator has to act on. Its one interpretive field is the
next operator action, and that is a total function of the rest of the view:
it grants nothing and decides nothing.

`autonomy events RUN` renders the persisted journal in persisted order. It
opens the durable store directly and takes neither the state directory's
exclusive ownership nor any run-driving lease, which is what makes it safe
and correct to read - or to tail - a run another controller is driving;
tailing is a poll of a local file that mutates nothing. Each line carries
journal identity, journal position, the chain links, and the exact state the
event is bound to when its payload names one. Artifact content is never
printed: what is rendered is the artifact reference the journal itself holds,
so listing events cannot emit raw local-only material, and a payload too
large to render is replaced by its size and digest rather than truncated into
JSON that no longer parses.

`autonomy doctor` answers, per capability, whether the thing a real run
depends on is actually there, and says what to do when it is not. PASS
states what was proven and carries its reason, so the report never implies a
fact from a status word. WARN is a question that could not be answered -
no repository was supplied, no forge adapter was authorized, a version string
did not parse - and FAIL is a dependency proven absent or proven unsafe. A
check that cannot be answered is WARN or FAIL, never a silent PASS. The
report's status is the worst check, and a failing report exits failed.

The diagnosis has no side effect that costs money or changes the world: no
provider inference call is ever made and no forge write is ever made. Its
single forge call is a read-only discovery read, and only when a forge
adapter and a repository were both explicitly configured. No secret reaches
the report - a credential is proven RESOLVABLE and the resolved value stays
in a local variable - and the provider credential path is inspected, never
read. The preflight does not repair what it measures, so a missing state
directory is reported rather than created, and it does not hold what it
diagnoses: the store is opened and closed, and the controller-instance lock is taken
and released, by the checks themselves. Owner liveness is probed as evidence
while the lock is held, and a platform that cannot decide whether the lock is
held is a FAIL, because without crash-safe evidence a dead owner could never
be proven dead and takeover would be blocked forever. Protected eligibility
is decided by the provider's own isolation report rather than by a
configuration string. Both configuration layers are re-read from disk,
because whether they still load strictly, still only tighten, and still
validate IS the check. The governance check is policy-authoring feedback and
grants nothing: it compiles the same predicted contract a real run would
compile and warns when policy grants publication only once the paths are
known, since the runtime cannot predict which files an issue will touch, that
grant would appear at reassessment as a privilege expansion, and reassessment
correctly refuses it.

`autonomy gc` reclaims heavyweight local material under the runtime state
directory, plan then delete. There is exactly one planner - a dry run prints
the plan and a real run executes it - so the two can never disagree about
what is eligible. Eligibility is proven twice, during planning and again
immediately before each deletion, because a run that was terminal and idle
when the plan was printed can be holding a lease by the time it is executed;
a target revalidation refuses is reported as skipped rather than silently
dropped. There is no metadata repair afterwards, because nothing the
collector deletes is the authority for anything: every target is heavyweight
material that a canonical row already explains by reference.

Never eligible: anything belonging to a run that is not terminal, so an
active or waiting run keeps its workspace, its checkouts, and its transcripts
and stays explainable; anything belonging to a run holding a leased or
running operation; anything younger than the retention window; the runtime
database, its sidecars, and every canonical row in it, which no delete path
can reach; a lock belonging to a runtime that cannot be proven dead; anything
whose ownership cannot be proven, such as a run directory with no durable run
row, an artifact path no journal event references, or a lock file whose name
is not a runtime owner identity; and anything resolving outside the canonical
state directory or reached through a symlink leaving it. What may become
eligible after retention: the candidate workspace of a completed, cancelled,
or failed run, its detached assurance checkouts, its raw local-only
transcripts, and the controller-instance lock of a provably dead runtime. Sanitized
artifacts are deliberately kept, because they are the durable explainable
derivative and they are small. The retention window is operator authority for
the same reason a budget ceiling is - a repository that could shorten it
would be choosing how long the material explaining a run against it survives.
