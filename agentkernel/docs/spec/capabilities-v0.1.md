# Capabilities and tools v0.1

Normative for Gate A. Code: `api/request.go`, `api/tools.go`, `api/validate.go`,
`tools/*`, `engine/tools.go`, `storage/artifacts.go`.

## 1. Principle

A model's tool call (`api.ToolCall`) is an untrusted proposal. Authority comes
only from a host grant (`api.Capability`) in the request. Nothing in a tool
name, arguments, tool output or context can create, widen or extend a grant,
enable a network endpoint or move a deadline. The host revalidates at
side-effect time using the grant `handle` echoed in `tool.executed` events and
`ToolResult.Grant`.

## 2. Grants

```go
type Capability struct {
    Handle   string         // opaque; echoed for host revalidation
    Kind     CapabilityKind // file.read | file.search | file.write | command.run
    Roots    []string       // file kinds only
    Commands []CommandGrant // command.run only: {Name, Argv, TimeoutSeconds}
}
```

Validation (`validateGrants`, `Capability.validateShape`), before any side
effect:

- `handle` is a valid identifier, unique in the request; `kind` is known.
- File kinds: at least one root, no commands; every root satisfies
  `api.ValidRelativePath` — slash-separated, `path.Clean`-clean, not absolute,
  no `..` component, no backslash, no NUL, no drive letter; `.` is the
  workspace root.
- `command.run`: at least one command, no roots; each command has a unique
  valid-identifier name, non-empty `argv` and `timeout_seconds > 0`.
- In `read_only` mode, `file.write` and `command.run` grants are refused.

The four kinds are the whole vocabulary. There is no non-file read kind, so a
host tool over other material (the A02 document tool in `examples/hostport`)
borrows `file.search` and its root scoping; and `command.run` exists only in
`read_write`, so a read-only execution cannot run even a non-mutating host
command. Both are known gaps, not oversights in the broker.

## 3. Broker (`tools.Broker`)

`tools.NewBroker(tools...)` refuses a nil tool, a name outside
`^[A-Za-z0-9_-]{1,64}$`, a duplicate name, an unknown kind, and an input
schema outside the enforceable subset (§3.2). Every tool implements
`Tool.Scope`; the interface requires it, since every capability kind is scoped.

**Offering** (`Broker.Specs(grants, mode)`): a tool is offered, in
registration order, only when at least one grant of its kind exists and the mode
permits the kind. The engine passes these specs to the provider and names them
in the boundary text.

**Admission** (`Broker.admit`), in order; any failure returns
`ToolResult{Status: refused}` and the tool is never invoked:

1. the tool name is registered;
2. `OutputLimit > 0`;
3. the mode permits the tool's kind (mutating kinds only in `read_write`);
4. the arguments match the tool's schema exactly (§3.2);
5. `Tool.Scope(arguments)` succeeds and names something: at least one path
   for file kinds, a command name for `command.run`;
6. some grant of the tool's kind covers the whole scope: every path lies under
   one of its roots (`rootFor`: lexical containment after
   `ValidRelativePath`, and no path component equal to `.git` compared
   case-insensitively — no grant reaches repository metadata), or the
   command name is in its `Commands`. The first such grant is selected.

**Invocation.** The tool receives `tools.Invocation` with the selected grant
only, plus `StepBudget` from `tools.Env` (the engine supplies `run.chargeStep`,
which charges one tool call per composite step after the first). A tool MUST
recheck its own guards at side-effect time rather than trust the broker.

**Errors.** A tool returning a Go error yields status `error`. If the tool's
kind is mutating, or the result says `Mutated`, the broker also returns an
"outcome unknown" error; the engine then settles `failed/tool_failed` and runs
nothing further. Status `error`/`refused` without a Dispatch error goes back to
the model as a tool message. Whenever the execution stops after a tool whose result says `Mutated`,
the engine records an extra `tool.executed` event with detail
`side effect outcome uncertain: …` before settling.

### 3.1 Output filtering (`tools.bound`)

When `Output` exceeds `OutputLimit` bytes, the broker records the exact full
output as an artifact (unless the tool already set `FullOutput`), cuts `Output`
at a UTF-8 boundary and sets `Truncated`. `Error`, `ExitCode` and `Mutated`
pass through unchanged. If the full output cannot be recorded, `Output` is
emptied, `Truncated` set, status becomes `error` with the reason, and after a
mutation the broker returns an unknown-outcome error. The model receives the
result as JSON with `status`, `grant`, `exit_code`, `truncated`,
`full_output`, `mutated`, `error`, `output` (`engine.renderToolResult`): the
evidence is never replaced by a lossy summary.

### 3.2 Enforced schema subset (`tools/schema.go`)

```text
{"type":"object","additionalProperties":false,"required":[...],
 "properties":{"<name>":{"type":"string"|"integer"|"boolean","description":"..."}
              |{"type":"array","items":{"type":"string"},"description":"..."}}}
```

Arguments are refused for unknown fields, wrong types, `null`, duplicate keys,
trailing data, missing required fields, and integers that are not plain
literals within int64. No keyword is shown to a provider and then left
unenforced.

## 4. File tools (`tools.Workspace`)

`tools.NewWorkspace(root, snapshot)` requires a clean absolute path to an
existing directory. Tools: `ReadFile()` → `read_file` (`file.read`),
`Search()` → `search` (`file.search`), `WriteFile()` → `write_file` and
`ApplyPatch()` → `apply_patch` (`file.write`), `ReadFiles()` →
`read_files` (`file.read`, §6).

### 4.1 Path guards (`Workspace.open`)

1. The requested path must lie under a granted root and contain no `.git`
   component, case-insensitively (`rootFor`, `isGitMetadata`;
   `TestGitMetadataIsNeverReachable`).
2. The workspace root is opened as an `os.Root`.
3. `refuseSymlinks`: every component of the granted root is `Lstat`ed;
   a symlink anywhere in it is refused — a grant names a directory, not
   wherever a link points.
4. The granted root is opened as a nested `os.Root`, and `sameDir` re-checks
   with `os.SameFile` that the held directory is still the named one, refusing
   a swap between check and open.
5. Every subsequent open, stat, mkdir and rename goes through that `os.Root`,
   which refuses at open time any path resolving outside the granted root.
6. `refuseSymlinks` again, below the granted root: every component of the
   requested path is `Lstat`ed through the `os.Root` before any open, and a
   symlink in any of them is refused, including one that stays inside the
   grant. `os.Root` alone follows in-root links, so without this a link that
   never names `.git` (`meta -> .git`, `cfg -> .git/config`,
   `src/x -> ../.git`) would alias repository metadata past the lexical check
   in step 1 (`TestSymlinkAliasToGitMetadataIsRefused`,
   `TestSymlinkInsideGrantIsRefused`).
7. `checkOpened`, after the open: the opened file must be a regular file and
   `os.SameFile` with an `Lstat` of its name, so a final component swapped for
   a link or another file between step 6 and the open is refused
   (`TestCheckOpenedRefusesSwappedFile`).
8. Hard links: on unix a regular file whose link count (`syscall.Stat_t.Nlink`)
   exceeds 1 is neither read nor written, and `search` skips it, because a
   second name aliases a file such as `.git/config` with no symlink anywhere
   (`TestHardLinkAliasToGitMetadataIsRefused`). Off unix the link count is not
   visible and hard links are **not** refused; that is a known limitation.

Reads accept only regular files of at most `tools.MaxFileBytes` (8 MiB).

Not guarded: the workspace root path itself may be a symlink (it is resolved
by `os.Stat`/`os.OpenRoot` at use); hard links off unix (step 8); a parent
directory swapped for a link by a concurrent same-user process between step 6
and the open (step 7 rechecks only the final component, and an `Lstat` through
the swapped parent agrees with the open); credential-shaped names and contents
are **not** refused by the kernel's tools (the root `runtime.ToolBroker`
refuses them through `GuardCandidate`; a Gate B gap, see
`docs/integration-plan.md`).

### 4.2 Snapshot guard

`tools.SnapshotGuard{Bound, Current}` is optional. When set, `read_file` and
`search` prefix their output with a note if `Current()` differs from `Bound`
or fails. This makes drift visible so stale context can be refreshed; it does
not refuse, and write tools do not consult it. It is a precheck, not
isolation.

### 4.3 Read and search

`read_file` output starts with `path`, `digest` (sha256 of the whole file —
the write precondition) and `lines a-b of n`, then the requested 1-based
inclusive range. `search` takes a literal or RE2 pattern of 1–1024 bytes, walks
regular files under one path, refusing a path that crosses a symlink (step 6)
and never following a symlink, entering a `.git` directory or reading a
hard-linked file below it; it skips binary (NUL),
unreadable and oversized files, echoes at most 300 bytes per line and stops at
1000 hits, stating that the result is incomplete.

### 4.4 Write preconditions

`write_file{path, content, expected_sha256}` and
`apply_patch{path, expected_sha256, old_text, new_text}`. `expected_sha256` is
the digest the caller last read, or `absent` (create only; `apply_patch`
requires an existing file and exactly one non-empty occurrence of `old_text`).
`Workspace.writeChecked`:

1. refuses a symlink in any component of the target path (§4.1 step 6) and
   a non-regular or hard-linked target (steps 7–8), so neither the target nor
   a parent directory can alias a file outside the grant;
2. compares the current digest with `expected_sha256`; a mismatch fails with
   "re-read before writing" and never overwrites;
3. bounds new content to 8 MiB; creates missing parent directories, then
   re-checks the whole parent chain for symlinks before staging;
4. writes a `.zk-<16 hex>.tmp` file with `O_EXCL` in the same directory,
   fsyncs and closes it;
5. re-checks the precondition, including step 1, so the rename target is
   still not a symlink, then renames over the target. On any failure the
   temporary file is removed.

A writer racing between step 5's check and the rename is not excluded; the
window is narrowed, not closed. `Mutated` is true after a successful write, and
also on failure if parent directories were created.

## 5. Commands (`tools.NewCommand`)

`tools.NewCommand(runner api.CommandRunner, dir string)` returns
`run_command` (`command.run`). The model supplies only `{"command": <name>}`;
argv and timeout come solely from the matching `CommandGrant`, and the process
runs through the host's `CommandRunner` in `dir` (a clean absolute path fixed
at construction). The kernel never spawns, contains or reaps a process.

- A runner error is an unknown outcome (`Mutated: true` plus an error →
  `failed/tool_failed`): a command's effect is never assumed absent.
- The full output (`exit_code`, stdout, stderr) is always recorded as an
  artifact; failure to record it is an unknown outcome.
- Status is `ok` only for exit code 0; otherwise `error` with
  `exit status N`. A runner-reported truncation is stated in `error`: the
  artifact then holds only the bytes the runner returned.

## 6. Macro: `read_files`

`Workspace.ReadFiles()` reads 1–16 paths in order. Admission requires
one `file.read` grant covering every path. Each step is dispatched through an
internal broker holding only `read_file`, under only the admitted grant, in
`read_only` mode, with `OutputLimit / len(paths)` (at least 1) and producer
`<producer>/step-<i>`; capability is therefore rechecked per step. The first
step rides on the macro call's own tool-call reservation and every later step is charged through `Invocation.StepBudget`
(the engine's `run.chargeStep`); a refused charge stops the macro with a
`budget:` step failure (`TestMacroStepsAreChargedToTheToolBudget`). Per-step
artifact bytes are metered as usual, so a macro cannot hide work from the
ledger.

Partial failure: execution stops at the first refused or failed step; the
result is status `error` naming the step and carrying every completed step's
output. Steps are idempotent reads, so a retry repeats no effect. A macro cannot
hide privileged work: it can only read.

## 7. Read-only structure

`read_only` is enforced three times: validation refuses mutating grants;
`Broker.Specs` never offers mutating tools; `Broker.admit` refuses mutating
kinds. The macro's inner dispatch is forced `read_only`.

## 8. Artifacts

`api.ArtifactRef{digest, size, media_type, producer}`. Tool output artifacts
use media type `text/plain; charset=utf-8` and producer
`<execution_id>/<attempt_id>/<call_id>` (engine-assigned). The engine wraps the
host store in `meteredStore`: it reserves artifact bytes before each `Put`
(refusal settles `exhausted/artifact_bytes`), refunds on `Put` failure,
appends every stored ref to the result, and emits `artifact.recorded`.

Stores (`storage`):

- `Get` verifies size and digest and returns `storage.ErrCorrupt` rather than
  corrupt bytes. `FileArtifacts` also verifies a JSON header line binding all
  four ref fields, and detects trailing bytes.
- The same bytes from two producers are two records (keyed by digest, size,
  media type and producer).
- Capacity is bounded (`maxBytes`); a write that would exceed it fails with
  `storage.ErrCapacity`. Stores never evict to make room.
- Retention: `Retain`/`Release` mark an artifact as referenced; `Delete`
  refuses a retained artifact (`storage.ErrRetained`). `FileArtifacts`
  persists marks as `.pin` files across restarts.
- `FileArtifacts`/`FileRecords` write atomically (temp file, fsync, rename,
  directory fsync) under an explicit absolute root and refuse Windows.
- `FileArtifacts.Put` counts an existing file for the same key as stored only
  if it verifies; a corrupt copy is rewritten in place without double-counting
  capacity.

## 9. Isolation claim

**Unproven, development-grade.** The guards above refuse every symlink and
(on unix) every hard-linked file on a requested path, but they narrow rather
than close races with a concurrent same-user process: one that swaps a parent
directory for a link between the per-component check and the open can still
redirect that open to another file inside the granted root (repository
metadata included, when the grant covers it), and off unix a hard link is
not detected (§4.1). `Provenance.Isolation` is the chosen binding's host
statement (`unproven` or `host_proven`); the kernel
itself never claims protected execution.
`Constraints.RequireHostProvenIsolation` makes routing admit only
`host_proven` bindings. A host that needs protected execution must supply it
(its own sandboxed `CommandRunner`, its own tools) and prove it.
