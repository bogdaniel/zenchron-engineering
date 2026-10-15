# ADR-0008: Governed Authoring and Reload of Planning Registry Artifacts

- Status: Accepted
- Date: 2026-10-15
- Issue: #471/#479 Phase 3 (Agent & Workflow Integration). Inputs: #64.
- Amends: ADR-0003's `planning_dir` row (§3) and its "stays C with no
  follow-up" list. Everything else in ADR-0003 is unchanged.

## The decision in one sentence

**`planning_dir` the PATH stays category C, exactly as ADR-0003 decided. The
FILES inside it were never part of that decision - ADR-0003 digests only the
path, "artifact content has its own digests" - so a governed API may write and
reload them without touching anything ADR-0003 froze.**

## Context

ADR-0003 classified `planning_dir` as category C: fixed at controller start,
part of the controller-effective digest, changed only through a cold
`controller re-adopt`. It listed `planning_dir` in "these stay C with no
follow-up." Read on its own, that line looks like it closes the door on any
governed mutation of what lives under that directory.

It does not. ADR-0003's own target-identity column for `planning_dir` says
exactly what is frozen: "as above [controller-effective digest, run ID,
PlanID]. **Artifact content has its own digests.**" The row's rationale is
"names the source of operator customization authority" - WHICH DIRECTORY, not
which bytes are in it today. `planning.LoadRegistry` already treats content as
separately identified: each artifact is sealed with its own content digest at
load time, and a plan revision pins that per-artifact digest, never a registry
snapshot or the directory's path. ADR-0003 reasoned about digest and freeze
points; it never addressed operator tooling for writing these files, and
"stays C with no follow-up" is a statement about the path's digest
classification, not a prohibition on how the files get there.

Today an operator creates or edits an `InstructionPack`, `ContextPolicy`,
`AgentProfile` or `EngineeringPlanTemplate` by hand-editing a JSON file under
`planning_dir/{instructions,context,profiles,templates}/<id>.json` and
restarting the process that calls `planning.LoadRegistry` (`serve`, or a local
`run`/`plan` invocation). `docs/planning.md` frames this as the complete,
correct operator workflow, and `ROADMAP.md` lists it as a shipped M2
capability - correctly, as far as it goes: the file mechanism itself is right,
for exactly the reason ADR-0003 gives (inline prose in the digested config
would re-identify every run in flight on every edit).

What is missing, and what #471/#479's goal asks for, is a way to create and
edit these artifacts from the operator's dashboard - validated, versioned,
durable, reachable without a text editor or a restart - without inventing a
second place these objects live. #64's milestone text is explicit that it adds
"no second runtime... no second task database": a parallel SQLite store for
these four types would be exactly that, and would also contradict the
"artifact content has its own digests" reasoning ADR-0003 already worked out
for exactly this directory. The file layout is not the problem; the absence of
a governed way to write to it, and of any way to pick up a write without a
full process restart, is.

## Decision

1. **`planning_dir` remains category C**, frozen at controller start, part of
   the controller-effective digest, changed only through #307 cold re-adopt.
   Unchanged from ADR-0003.
2. **The file layout, format and per-artifact content digest remain the
   source of truth.** No new store, table or journal is introduced for
   `InstructionPack`, `ContextPolicy`, `AgentProfile` or
   `EngineeringPlanTemplate`. A governed write is a second AUTHOR of the exact
   same sealed JSON shape `planning.LoadRegistry` already reads and
   `schemas/*.schema.json` already validates - never a parallel
   representation.
3. **A new governed verb validates and atomically writes one artifact** into
   its existing subdirectory, by id, through the same `#398` governed-action
   contract (controller-binding proof, CSRF, idempotent-reply handling)
   `EngineeringPlan` and `Product` mutations already use. It is refused,
   before anything is written, by the identical seal/schema validation
   `LoadRegistry` applies on read - an operator cannot write through the API
   what the loader would refuse on restart.
4. **A new governed verb reloads the in-process registry**: re-run
   `LoadRegistry` against the unchanged `planning_dir` path and atomically
   swap the held value. This is safe without a #307 re-adopt because the
   swapped content is never digest-bearing identity under ADR-0003 - only the
   path is, and the path does not change. It is exactly as safe as the
   existing "edit the file, restart the process" workflow; it only removes
   the restart.
5. **Assignment binding is unaffected.** A plan revision, once proposed,
   keeps pinning the exact profile/pack/template digest it was compiled
   against (already true today). A later write or reload changes what a
   FUTURE revision may select; it never moves what an approved or in-flight
   plan already pinned.
6. **Out of scope here**: a visual workflow editor (Slice D, separate), any
   change to how `planning_dir` itself is selected or digested, and any
   change to #64's compiler. This ADR adds one authoring and reload surface
   above an unchanged storage and compilation layer.

## Consequences

- An operator creates and edits reusable agent profiles, instruction packs,
  context policies and plan templates from the dashboard, matching the
  goal's "reusable, versioned, durable, validated, editable through governed
  APIs" without hand-editing JSON or restarting `serve`.
- The file layout stays the audit-visible, hand-inspectable, git-diffable
  source of truth an operator could always fall back to by editing it
  directly; the API and the text editor write the identical format.
- No second storage system, no second validation path, and no weakening of
  the digest-pinning a plan revision already does.
- `ROADMAP.md`'s "what #64 implemented" line undersold what the dashboard
  needs: the file mechanism was correct, but "operator-owned" had been read
  as "hand-edited," not "not candidate-supplied." This ADR does not reopen
  #64's architecture; it adds the authoring surface #64 left for later.

## Questions resolved by the implementing PRs

1. **Reload is an explicit, operator-triggered verb** (`ControlPlanningReload`
   / `POST /v1/planning/reload`), not automatic after every write. This
   matches "no hot reload" elsewhere in ADR-0003's S category and keeps a
   write's effect on in-flight plan proposals observable and intentional.
2. **No optimistic-concurrency rule, and no retained prior version, for
   these four artifact kinds.** `Product` and `EngineeringPlan` are
   revision-counted state machines another object pins an exact revision
   of, so a stale write there would silently move a reference something
   else depends on - `AdoptProductRevision`'s "next must be current + 1" CAS
   exists for that reason. An `InstructionPack`, `ContextPolicy`,
   `AgentProfile` or `EngineeringPlanTemplate` is not: nothing pins "profile
   X at version N," a plan revision pins the exact per-artifact CONTENT
   DIGEST it was compiled against (decision point 5, above), so a later
   write - from any source, including a second operator - only changes what
   a FUTURE proposal may select and never moves what an approved or
   in-flight plan already resolved. A write is therefore idempotent by
   content (the identical document reseals to the identical digest and
   overwrites with identical bytes) and a differing write to the same id is
   an ordinary, intentional replacement of a reusable input - exactly as
   consequential, and exactly as uncoordinated between two editors, as the
   hand-edit-the-file workflow this API sits beside. Adding CAS or a
   retained revision history here would duplicate `Product`'s machinery for
   an object this ADR's own reasoning says does not need it.
