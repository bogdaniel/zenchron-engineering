# Benchmark methodology (Gate A)

This is how `cmd/kernel-eval` measures the Agent Execution Kernel against the
paired baselines of issue #446 section 16, what each reported number means, and
what it does not mean. The hypothesis under test is that repeated work on the
same repository can become cheaper or faster **without degrading independent
outcome quality**. Gate A does not prove that hypothesis. It proves that the
mechanics it depends on (cache reuse, invalidation, memory staleness, budgets,
capability refusals) work and can be measured honestly.

## Reproduce

From `agentkernel/`, with no network and no credentials:

```bash
export GOWORK=off
go run ./cmd/kernel-eval -corpus testdata/eval -trials 3 -out /tmp/kernel-eval.json
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-corpus` | `testdata/eval` | directory holding `corpus.json` and `fixtures/` |
| `-trials` | `3` | trials per task and mode |
| `-out` | stdout | report path |
| `-mode` | `all` | `all`, or a comma list of `baseline,cold,warm,drift` |

The exit status is 0 when every planned run executed and verified. It is
nonzero only for a harness error (a run that could not be executed or
measured) or a run whose termination or verification checks differ from the
task's expectation. **There is no savings threshold.** CI runs exactly the
command above in `.github/workflows/agent-kernel.yml`. `go test ./cmd/...`
also runs the whole corpus once per mode and checks that an altered
expectation is reported.

## Corpus

`testdata/eval/corpus.json` is the whole task corpus. Its digest is in every
report.

- **Fixture.** `testdata/eval/fixtures/calc` is a small Go module (`go.mod`,
  three packages, a test, a build-tagged file, a JSON config, a Markdown
  note). A directory that contains `go.mod` is ignored by the `go` tool in
  this module, so the fixture is never built as part of `agentkernel`. The
  report records each fixture's content-manifest digest.
- **Tasks.** Each task has an objective, a tool mode, host grants, an optional
  partial budget override, scripted provider steps, an expected termination
  (outcome, cause, budget dimension) and verification checks. Nine tasks: six
  held-in (`t01`-`t06`) and three held-out (`h01`-`h03`). Between them they
  cover a read, a patch with a reread after it, a new file, search then patch
  of config, a refused ungranted write, a retried transient provider failure,
  a stale write precondition, a redundant reread, and tool-call budget
  exhaustion.
- **Scripted steps carry no token usage.** The scripted provider reports only
  what its script says, and the corpus says nothing, so every reported token
  count is unknown. Writing counts into the script would invent them.
- **`{{sha256:path}}`** in a step argument is replaced, at run start, by the
  digest of that file in the run's workspace. It stands for the precondition a
  model copies from its own earlier `read_file` output.
- **Checks.** `unchanged` (bytes equal to the run's starting bytes),
  `file_contains`, `file_absent`, `observations` (exact count of an event kind
  in the result) and `retries` (exact provider retry count). A run is
  *verified* only when its termination matches the expectation and every check
  passes.
- **Drift scenarios** are named in the corpus and each task names one:
  `source_edit` (a package gains a function), `config_edit` (a non-Go file
  changes), `go_mod_change` (the `go` directive changes) and `build_tag`
  (extraction settings gain the `extra` tag; no file changes).

## Paired baselines

Every task runs in every selected mode, in this order, in every trial, each
run on a **fresh copy** of its fixture (tasks write files). The engine, tool
broker, artifact store and scripted provider are rebuilt for every run.
Within one (trial, task) pair the cold, warm and drift runs share file-backed
stores in a scratch directory, which the harness deletes on exit.

| # | Mode | Context sources | Index | Memory |
| --- | --- | --- | --- | --- |
| 1 | `baseline` | none | not built | not used |
| 2 | `cold` | index view + memory | empty `storage.FileRecords`; `intelligence.Open` builds and caches | empty store |
| 3 | `warm` | index view + memory | the same directory **reopened** (a simulated restart); `Open` must validate and hit | reopened; holds the cold run's record |
| 4 | `drift` | index view + memory | base snapshot reloaded by key, `Overlay` applies the drift; a fresh `Open` of the drifted bytes is timed beside it | records derived from changed files invalidated |
| 5 | native CLI vs kernel | - | - | **not run: Gate B** |

- The workspace digest bound into each request is the intelligence manifest
  digest. In the baseline the harness computes the same manifest shape
  itself. `manifest_agrees` records that both agree.
- When `warm` or `drift` is selected without `cold`, the harness first fills
  the cache from a pristine copy. That setup is not an observation.
- In `drift`, `overlay_matches_rebuild` is true only when the overlay's
  identity key equals the key of a fresh build of the drifted bytes **and** that
  fresh `Open` did not hit the stale cache.
- **Memory and held-out tasks.** After the cold run of a held-in task the
  harness writes one observation record ("an earlier run of X read ... and
  settled ...") bound to the digests the run read. Held-out tasks never get a
  record, so no remembered summary of a task can leak into its own later runs
  and pass for general improvement. Held-out results are reported separately.

## Recorded per run (`observations[]`)

Every raw observation is kept in the report. Nothing below is extrapolated.

| Field | Meaning | Does not mean |
| --- | --- | --- |
| `outcome`, `cause`, `dimension` | the kernel's single settled termination | that the objective was achieved, or that any change can be accepted |
| `expected_matched`, `checks`, `verified` | the termination and fixture checks against the task's expectation | independent engineering assurance |
| `wall_ms` | wall time of one `Engine.Execute` only | end-to-end cost: index time is reported separately |
| `index.open_ms` | time for `Open` (cold/warm) or `Load`+`Overlay` (drift) | a large-repository figure: the fixture is tiny |
| `index.manifest_ms`, `extract_ms`, `files_hashed`, `files_parsed`, `dirs_extracted`, `dirs_reused`, `cache_hit`, `cache_degraded`, `invalidation` | `intelligence.Stats` and the overlay binding | |
| `index.rebuild_ms` | a full fresh `Open` of the drifted workspace, for comparison with the overlay | |
| `retrieval[]` | time and item count of each context source, measured by wrapping the source | |
| `context_selected_by_kind` | context-manifest entries selected, by kind | that the model used them: the scripted model cannot |
| `memory.records_written`, `invalidated`, `items_selected` | memory writes after cold, records made stale by drift, memory items selected into context | |
| `derived_store_bytes` | on-disk bytes of the index and memory stores when the run starts | |
| `reported_tokens` | provider-reported counts; `null` is unknown | zero |
| `estimated_tokens` | the engine's local approximation (about four bytes per token) of the compiled prompts | billing data |
| `provider_calls`, `tool_calls`, `retries` | the engine's usage account | |
| `tool_output_bytes_to_model` | bytes of tool-result messages in the final provider request | bytes read from disk |
| `files_read`, `rereads` | distinct paths read through `read_file`/`read_files`, and repeat reads split into *justified* (the digest changed since the last read) and *redundant* (identical digest) | search scans, which are not counted; `null` means unknown |
| `alloc_bytes` | process-wide allocation delta around `Execute` (sequential runs) | per-run resident memory |
| `unknowns` | every dimension this run did not establish | |

Transcript-derived fields come from the transcript delivered to the provider.
A tool result produced after the final provider call (for example at budget
exhaustion) never reached the model and is not counted.

## Report shape (`agentkernel.kernel-eval.report/v0.1`)

- **Bindings:** kernel version, Go version, GOOS/GOARCH, command, config and
  config digest (trials, modes, extraction settings, binding limits, deadline,
  memory limits), corpus digest, fixture digests, and one script digest per
  task.
- **Denominators:** `planned` (tasks x trials x modes), `attempted`,
  `executed`, `harness_errors`, `verified`, `verification_failed`, and kernel
  outcome counts. Counts, never percentages.
- **`by_mode`:** for each mode, `held_in` and `held_out` groups with their own
  denominators, min/median/max spreads over the runs where a value is known
  (with an `unknown` count), and summed counters (rereads, retries, cache hits
  and degradations, overlay agreement, memory items and invalidations). A
  metric that does not apply to a mode is omitted from that mode.
- **`unknowns`:** every unknown dimension and how many runs lacked it.
- **`failures`:** every harness error, unexpected termination and failed check.
- **`not_run`:** baseline 5, with the reason.
- **`limitations`:** stated in every report (below).

## What a number means

- **Cold vs warm.** Compare `index.open_ms`, `files_parsed` and `cache_hit`.
  Warm reuse shows that a validated snapshot is served without re-extraction.
  `wall_ms` should not differ much, because `Execute` does not include index
  time.
- **Drift.** Compare `index.open_ms` (reload + overlay) with
  `index.rebuild_ms`, and read `invalidation`, `dirs_extracted` and
  `dirs_reused` to see what the change cost. `overlay_matches_rebuild` is the
  correctness check. A false value is a defect, not a slowdown.
- **Estimated tokens rise with context.** Cold, warm and drift runs add
  retrieved context, so they estimate more input than the baseline. That cost
  is real and is reported. With a scripted model nothing offsets it.
- **Equal outcomes across modes** show only that optional context and memory
  did not change execution under these fixtures. Scripted actions are fixed,
  so this cannot show a quality gain or loss.

## Limitations

The report carries these in its `limitations` array:

- Offline fixtures prove mechanics, not paid-model quality or accepted
  engineering throughput.
- No reported token or cost figure exists for the scripted provider, so no
  saving is claimed for any provider.
- No pass threshold such as "35% savings" exists.
- Wall times are from single-machine sequential runs. The fixture is tiny.
- CPU time, resident memory, interventions and maintenance cost are not
  measured.
- Held-out tasks never receive remembered summaries of themselves.

## Gate B: baseline 5 (native CLI vs kernel)

Item 5 compares existing native CLI/provider execution with the kernel path. It
spends provider credit and needs the production host, so it is out of Gate A
scope. It must:

1. Run under the host's integration (#445 boundaries), on **isolated identical
   task snapshots** (same workspace manifest digest, same objective, same
   grants and budget) for both paths.
2. Judge outcomes with **host-owned assurance**: the existing Engineering
   Authorization Kernel evidence and authority path, never the kernel's
   `completed` outcome and never the implementing agent's own claim.
3. Be approved for spending in advance, with a monetary ceiling enforced
   through trusted pricing (`api.MoneyCeiling`), and record actual cost with
   currency, rate source and rate version.
4. Pin and record model identity, version (or explicit `unknown`), adapter
   configuration fingerprint and prompt/instruction digests for both paths.
5. Use provider-reported usage only, keep local estimates separate, and record
   unknowns. A provider that reports no usage gets no saving claim.
6. Run enough trials to report variability. Keep held-out tasks, and report
   failed attempts, rework, independent verification and maintenance in total
   cost.
7. Gate adoption on a reproducible benefit with no governance regression and
   an explicitly stated quality threshold and measurement limit, not on a fixed
   percentage.
