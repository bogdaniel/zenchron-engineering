# ADR-0002: Local Engineering Runtime

- Status: Proposed (candidate; not adopted)
- Date: 2026-08-29

## Decision

The local runtime is a durable reconciler above the Authorization Kernel.  It
persists append-only run events and bounded operations outside the target
repository, folds events deterministically into a run snapshot, and only then
plans eligible operations.  Providers and GitHub adapters are replaceable
adapters; neither can authorize protected actions or own commits, pushes, PRs,
or merges.

Candidate producers use a runtime-owned clone with independent Git metadata.
Every candidate mutation is committed by the runtime, reassessed, and verified
from a fresh exact-tree checkout.  Runtime state, credentials, raw transcripts,
and leases never live in the target repository.  Repository configuration can
tighten operator safety settings but cannot loosen them.

The operator surface above the reconciler is a projection and a recorder, not
a second governance layer.  It records human decisions as typed evidence bound
to exact state and reports what the kernel's authority evaluator concludes; it
assigns no authority of its own.

### Execution trust boundary

Provider inference connectivity is not candidate command connectivity:

```text
provider inference connectivity/auth  !=  candidate command connectivity/credentials
```

Three capabilities are bounded separately rather than collapsed into a single
sandbox.

The provider control plane may authenticate explicitly to its configured AI
provider and may hold the connectivity that remote inference inherently
requires.  It receives no GitHub, SSH, signing, cloud, or application
credentials, and its result is an observation that carries no acceptance
authority.

Candidate and tool execution is confined to the bounded candidate workspace.
It inherits no ambient host environment, receives no GitHub, SSH, signing,
cloud, or application credentials, is denied network by default, and reaches
neither the runtime database, the controller checkout, nor another run.  A
candidate-modified `AGENTS.md` can never be promoted into trusted instructions
in the same run.

Assurance is unchanged and is not weakened by this split.  Every verifier
invocation receives the exact detached candidate tree rather than a producer
workspace, holds no AI-provider credentials, prepares dependencies through a
separately constrained step, and runs `gofmt`, `go vet`, and `go test`
offline.

The M0 Codex adapter is a native compound provider driving the installed Codex
CLI.  A Codex control process with no network and no authentication cannot
perform remote inference, so an isolation model that demands one is not
implementable.  What that adapter bounds, however, is not what the protected
boundary above requires.  Local Codex `--sandbox workspace-write` with
`sandbox_workspace_write.network_access=false` primarily bounds writes; it
does not establish bounded reads.  Provider tool execution under that mode is
not proven unable to read the runtime database and runtime state, runtime
locks, the controller checkout, another run's state, provider credentials, or
unrelated home data.  Filesystem read isolation for the native Codex adapter
is therefore unproven, and the required filesystem confidentiality is not
weakened to make the adapter appear compliant.

The adapter is retained explicitly as a bootstrap, operator-trusted adapter,
and it is ineligible for protected autonomous execution.  Eligibility fails
closed: a provider that does not prove the required isolation properties, or
does not report them at all, is refused rather than assumed compliant.

What the adapter does prove stays documented and is not overstated.  It probes
provider capabilities fail-closed and never silently uses full-access or
unsandboxed Codex.  Runtime-owned policy defines the effective sandbox
constraints; arbitrary user Codex configuration does not.  The candidate
command environment is an allowlist built from scratch rather than an
inherited ambient environment.  Provider authentication is a control-plane
capability and is never copied into a candidate command environment.  A
candidate `AGENTS.md` is left unmodified and can never become trusted
instructions in the same run.

The protected provider therefore separates reasoning from tool execution:

```text
reasoning provider
    -> tool request
    -> Zenchron-controlled ToolBroker
    -> candidate operations
```

Zenchron validates and executes each requested capability; the reasoning
provider never touches the filesystem directly.  The initial capabilities are
repository read, repository search, candidate diff, apply patch, and run
bounded command.  Candidate command execution continues through the existing
sandbox and receives the candidate workspace only: no runtime state, no
controller checkout, no other run's state, no provider credentials, and
network disabled by default.  Provider credentials remain solely in the
control-plane process.

That brokered adapter exists.  The OpenAI Responses provider runs its
reasoning loop in the trusted controller process, and the model emits only
tool names and JSON arguments: there is no hosted shell, no code interpreter,
no file-upload channel, and no path handle crossing the boundary.  Its API key
is an operator-supplied file reference, read inside the call and used only as
a control-plane request header; a key file resolving inside the candidate
workspace is refused, so repository content can neither supply nor redirect
the credential.  It is the provider that reports all four required isolation
properties as proven and is therefore the protected-eligible one.  Eligibility
still follows from what a provider reports rather than from which adapter it
is, and it still fails closed.  Provider completion remains an observation and
asserts nothing about acceptance.

Docker remains a valid isolation primitive for assurance and for providers
whose control and tool architecture supports it.  Providers are not required
to be Docker-shaped, and `ExecutionProvider` remains provider-independent.

### Operation persistence

Bounded operations are persisted in SQLite, in `runtime.db` under the operator
state directory.  A process-local mutex over a read-modify-write JSON document
is not a durable store: it protects a single process and therefore cannot
provide the atomic lease and idempotency guarantees the runtime requires.  An
in-process store remains a test double only.

The database carries an explicit schema version applied through ordered
migrations, and the runtime refuses to open a database whose schema version is
newer than the binary supports.  Each operation is a durable canonical-JSON
`RunOperation` document with a unique durable run-scoped idempotency key.
Creation, lease acquisition, heartbeat, state transition, and lease clearing
are transactional; attempt and budget updates are atomic; row revisions are
updated by compare-and-swap so two scheduler processes cannot both believe
they acquired the same operation.  Queue ordering is deterministic and
survives restart, and crash/restart recovery is a store obligation rather than
a caller convention.  Owner identity binds host, process ID, and process-start
identity; lease takeover still requires owner liveness, and wall-clock expiry
alone never authorizes stealing a lease.

The same operational state also holds the persistent run identity and the
append-only event journal, under the same schema-version and ordered-migration
mechanism.  Sequence numbers are allocated monotonically per run, an append is
transactional, and a duplicate or otherwise invalid sequence is refused rather
than reordered or overwritten.  The journal's hash chain is persisted and
verified, and deterministic replay rebuilds the run snapshot after the
database is reopened.  Canonical event rows hold references only; raw
transcripts never live in them.

Event identity is durable and caller-owned, while sequence, chain links, and
state digests are allocated inside the append transaction.  Two independent
journal writers over the same database - a watch controller holding the run's
operation lease and an operator command holding none - can therefore append
concurrently without colliding and without a gap, and neither derives identity
from an in-process counter or from the clock.  Owner liveness is evidenced by
an OS advisory lock held for a runtime instance's whole lifetime and released
by the kernel on death; a lock file is never liveness evidence, and a platform
that cannot decide whether the lock is held reports the owner alive so that
takeover is blocked rather than guessed.  The trusted candidate Git metadata
baseline is likewise persisted: every successful runtime-owned Git operation
journals the digest it established, and integrity is asserted against that
journalled baseline rather than against the live repository, so a restart
cannot launder metadata tampered with while no runtime was running.

### Human authority and the operator surface

`autonomy authorize` records evidence, not authority.  It appends one typed
human-authority event bound to exact state, and the existing action-scoped
evaluator decides what that evidence means; recording assigns no decision
status, writes no authority evaluation, and changes no phase or disposition.
Recording an approval and still reading `blocked`, `incomplete`, or `stale` is
an ordinary outcome.  Human approval cannot expand contract permissions, grant
a privilege expansion, bypass policy, make stale evidence current, or replace
independent assurance: it satisfies only outstanding claims whose evidence
class is human approval.  It cannot be given past a controller mismatch, a
moved source, an externally changed candidate head, or a terminal run, because
those gate recording as well as reading.  A rejection is recorded as failed
evidence rather than as an absence, so it blocks rather than merely remaining
unsatisfied.

The request an operator answers is a projection of current runtime state plus
the exact missing human-authority requirement, regenerated on every read and
never persisted or consulted as a permission.  Its identifier digests the
exact bindings - run, action, repository, controller, pinned source, base,
candidate revision and tree, contract revision, and the current decision and
evidence context - so an operator names exact state rather than typing SHAs,
and a binding added later is inside the digest by default.  Any material
change produces a different identifier; the old approval is refused as stale
rather than retargeted, and recorded authority is admitted as evidence only
for the contract and candidate revisions it was given against, which are never
rebound.  Recording is idempotent by content: the evidence identifier is the
journal event id and therefore the database's primary key, so racing operator
processes converge on one record and a crash retry adopts its own.

A candidate Zenchron cannot authorize its own adoption.  The human-authorizable
actions are an allowlist - open a pull request, update one, push the run-owned
candidate branch - and adoption is not on it, so a merge or adoption action is
refused as unsupported rather than given a shape that looks governed.  There is
no `autonomy merge` and no automatic pull-request merge; a merged pull request
is only an observation.  Final adoption of the runtime's own candidate remains
external to the candidate runtime.

Operator identity is recorded provenance, not verified person identity.  A
record names the configured identity, the local account, and the host, and
carries an explicit provenance value stating that none of it was
authenticated; nothing is signed and no challenge was issued.  Resolution
fails closed, and identity is an operator-layer member the repository layer
may not name, so a repository cannot choose who is recorded as authorizing a
change to it.

Changed source intent stops a run rather than silently redirecting it.
`autonomy refresh` is the explicit path that re-reads it and grants nothing:
no permission, no privilege expansion, no publication.  It is a separate
command rather than a flag on resume, which is what makes "resume never
absorbs changed source intent" structural.  It records refresh intent under a
reason distinct from an operator stop and draws a generation boundary, which
is what stales the prior contract, evidence, and authority; the prior journal
is preserved exactly.  Stopping the watch controller stops watching: no
cancellation is journalled and every run stays resumable.  `autonomy stop` is
the only durable cancellation of one run, and it reuses the scheduler's
existing cancellation request rather than introducing a second mechanism.

`doctor` answers each capability question independently, and a question it
cannot answer is WARN or FAIL rather than a silent PASS.  It makes no
inference call and no forge write, resolves credentials without placing one in
the report, and does not repair or hold what it measures.  `gc` reclaims only
heavyweight material that a canonical row already explains by reference: never
a non-terminal run's material, never a leased run's, never anything inside the
retention window, never the database or a canonical row, and never anything
whose ownership cannot be proven.

### Configuration authority

Configuration is a lattice, not a merge.  The operator layer alone may
authorize a credential, name the provider, name the assurance image, choose
the state directory, enrol repositories, set the opt-in label and the
retention window, and name the operator identity.  The in-repo layer may only
tighten: it can lower a bound and never raise one, and it can raise no
capability, concurrency ceiling, budget, credential authority, or publication
authority.  The members it may name are an allowlist enforced before decoding,
so adding a field cannot by itself grant a repository new authority, and a
proposal that would raise a bound is refused rather than clamped.  Both layers
decode strictly and digest canonically, so a run records the configuration
that governed it.

### Determinism and seams

The event and state hashes use RFC 8785 JCS.  The runtime serializes its typed
objects with Go's `encoding/json`, rejects values that would silently replace
invalid UTF-8 or exceed the I-JSON interoperable integer range, and passes the
valid JSON bytes to `github.com/gowebpki/jcs.Transform` before SHA-256.  The
JSON serializer is input preparation only; it is not claimed to be canonical.
A state digest excludes journal cursor and hash-chain fields; an event digest
excludes its own digest.  The reducer has no clock dependency.  Schedulers
inject time for leases and budgets.

Executable repository extensions and checkpoint import/export remain seams
only.  No extension command is executed and no forge comment is imported as
runtime state in M0.

## Consequences

Separating the control plane from candidate execution costs a second
enforcement surface.  Absence of candidate connectivity and credentials must
be proven per command rather than inferred from a process that had no network
at all, and the runtime must keep provider authentication out of every
environment it hands to candidate tooling.

A native provider depends on a correctly installed and authenticated host CLI,
so provider availability becomes an operator precondition rather than a pinned
immutable image identity the runtime controls.  Capability probing becomes a
fail-closed precondition of execution: the runtime must establish and prove
the sandbox mode and workspace boundary it asked for before any candidate
command runs, and a provider that cannot report those capabilities cannot be
used at all.  Because that probe cannot establish bounded reads, the native
adapter buys bootstrap capability rather than protected-execution eligibility,
and the protected path costs a broker the runtime must implement and maintain
itself instead of delegating tool execution to the provider.

A SQLite store adds schema versioning and migration obligations, and a binary
older than the on-disk schema refuses to run rather than degrading.  In
exchange, leases, idempotency, and queue ordering hold across processes and
across restarts instead of only within one process.

Binding an approval to exact state costs the operator a re-approval whenever
the subject moves, and the runtime must show the request rather than expect
anyone to reconstruct it.  That is the price of an approval that cannot be
carried onto work it was never given for.  Refusing self-adoption costs a
manual final step for the runtime's own changes, which is the intended cost:
the alternative is a system whose acceptance authority is the thing under
change.  Recording operator identity as unverified provenance costs the
ability to attribute a decision to a proven person, and it is recorded that
way rather than implied to be more.
