# Gate A benchmark results

Raw report: [`results-gate-a.json`](results-gate-a.json). Every number below is
copied from it. Nothing is extrapolated. Method:
[`methodology.md`](methodology.md).

## Run binding

| | |
| --- | --- |
| Date | 2026-10-07 (`generated_at` 2026-10-07T12:36:04Z) |
| Command | `go run ./cmd/kernel-eval -corpus testdata/eval -trials 3 -out docs/benchmarks/results-gate-a.json` (from `agentkernel/`, `GOWORK=off`) |
| Base commit | `4696e31d3dbd5aefe864d982e3a7f4e2af6eb9a2`, with the uncommitted Gate A lane work in the shared worktree |
| Kernel | `agentkernel/0.1.0-gate-a` |
| Go | `go1.27.1` toolchain (module declares `go 1.25.0`) |
| Machine | darwin/arm64, Apple M3, 8 cores, 24 GiB |
| Extraction settings | `go1.25.0` linux/amd64, cgo off (fixed, not read from the host) |
| Config digest | `sha256:9627784c7e865f74d5c3e804b723f9ad99d8fe80ca046785c6f6f08cd60586c4` |
| Corpus digest | `sha256:24bef1308e86eb83c7389d3061d93d419e2330be6cdd5d3a4fc19f7ad20fc4e0` |
| Fixture `calc` | `sha256:a2cb75a1f61d0526426d4f0001e1f0dcac3229fe0912b9ca6173f361edab20fb` |
| Script digests | one per task, in `corpus.script_digests` |
| Total duration | 1146 ms |

## Denominators

9 tasks (6 held-in, 3 held-out) x 3 trials x 4 modes:

| planned | attempted | executed | harness errors | verified | verification failed |
| --- | --- | --- | --- | --- | --- |
| 108 | 108 | 108 | 0 | 108 | 0 |

Kernel outcomes: 96 `completed`, 12 `exhausted`. All 12 exhausted runs are
`h03-tool-budget` (3 trials x 4 modes), whose expected termination is
`exhausted/budget_exhausted/tool_calls`. `failures` is empty.

Baseline 5 (native CLI vs kernel): **not run: Gate B.**

## Unknowns

These were unknown in all 108 runs: `reported.input`, `reported.output`,
`reported.cached_input`, `reported.cache_write_input` (the scripted provider
reports no usage), `estimated.cached_input`, `estimated.cache_write_input`,
`cost`, `cpu_time`, `resident_memory` and `interventions`. Files read and
rereads were known in every run.

## Per mode, held-in tasks (6 tasks x 3 trials = 18 runs per mode)

Each cell is min / median / max over the 18 runs.

| Metric | baseline | cold | warm | drift |
| --- | --- | --- | --- | --- |
| `wall_ms` (Execute only) | 0.048 / 1.261 / 5.187 | 0.080 / 1.929 / 6.262 | 0.055 / 1.465 / 5.348 | 0.066 / 1.809 / 4.892 |
| `index.open_ms` | - | 5.792 / 6.715 / 10.457 | 0.483 / 0.599 / 1.352 | 0.213 / 0.526 / 3.185 |
| `index.rebuild_ms` | - | - | - | 4.889 / 7.052 / 8.895 |
| retrieval ms | - | 0.024 / 0.056 / 1.070 | 0.020 / 0.093 / 0.302 | 0.020 / 0.101 / 0.830 |
| estimated input tokens | 702 / 1050.5 / 2660 | 956 / 2660.5 / 6767 | 1034 / 2855.5 / 7241 | 1034 / 2660.5 / 7241 |
| reported input tokens | unknown (18) | unknown (18) | unknown (18) | unknown (18) |
| tool calls | 1 / 1 / 3 | 1 / 1 / 3 | 1 / 1 / 3 | 1 / 1 / 3 |
| tool output bytes to model | 113 / 323.5 / 955 | 113 / 323.5 / 955 | 113 / 323.5 / 955 | 113 / 323.5 / 955 |
| files read | 0 / 1 / 1 | 0 / 1 / 1 | 0 / 1 / 1 | 0 / 1 / 1 |
| derived store bytes | - | 8939 / 8939 / 8939 | 8939 / 9528 / 9548 | 17878 / 18473 / 19535 |
| rereads justified / redundant | 3 / 0 | 3 / 0 | 3 / 0 | 3 / 0 |
| provider retries | 3 | 3 | 3 | 3 |
| cache hits | - | 0 | 18 | 0 (overlay, not cache) |
| cache degraded | - | 0 | 0 | 0 |
| overlay matches rebuild | - | - | - | 18 of 18 |
| memory items selected | - | 0 | 12 | 9 |
| memory records invalidated | - | 0 | 0 | 3 |

## Per mode, held-out tasks (3 tasks x 3 trials = 9 runs per mode)

| Metric | baseline | cold | warm | drift |
| --- | --- | --- | --- | --- |
| `wall_ms` (Execute only) | 0.102 / 0.141 / 0.299 | 0.190 / 0.225 / 0.514 | 0.155 / 0.345 / 0.923 | 0.170 / 0.226 / 0.382 |
| `index.open_ms` | - | 6.439 / 7.410 / 9.102 | 0.509 / 0.676 / 1.369 | 0.180 / 0.530 / 1.007 |
| `index.rebuild_ms` | - | - | - | 4.768 / 6.530 / 8.859 |
| estimated input tokens | 297 / 1068 / 1463 | 932 / 2724 / 5201 | 932 / 2724 / 5201 | 932 / 2724 / 5201 |
| reported input tokens | unknown (9) | unknown (9) | unknown (9) | unknown (9) |
| tool calls | 1 / 1 / 2 | 1 / 1 / 2 | 1 / 1 / 2 | 1 / 1 / 2 |
| files read | 0 / 0 / 2 | 0 / 0 / 2 | 0 / 0 / 2 | 0 / 0 / 2 |
| rereads justified / redundant | 0 / 3 | 0 / 3 | 0 / 3 | 0 / 3 |
| cache hits | - | 0 | 9 | 0 |
| overlay matches rebuild | - | - | - | 9 of 9 |
| memory items selected | - | 0 | 0 | 0 |

## What this run shows

- **Execution is unchanged by optional context.** All 108 runs reached their
  expected termination and passed their checks in every mode, including the
  refused ungranted write (`t05`), the stale write precondition (`h01`) and
  tool-budget exhaustion (`h03`).
- **Warm reuse works.** All 27 warm runs hit the reopened file-backed cache
  and parsed 0 files (cold runs parsed 5). Index open median: 6.715 ms cold vs
  0.599 ms warm (held-in), and 7.410 ms vs 0.676 ms (held-out), on a 9-file
  fixture.
- **Drift invalidation is scoped and correct.** In all 27 drift runs the
  overlay's identity equalled a fresh build of the drifted bytes, and the fresh
  `Open` never hit the stale cache. Invalidation per scenario:
  - `config_edit`: `files`, 0 directories re-extracted, 3 reused.
  - `source_edit`: `files`, 2 re-extracted (the edited package and its
    importer), 1 reused.
  - `go_mod_change`: `module`, 3 re-extracted.
  - `build_tag`: `all`, 3 re-extracted.

  Median overlay cost was 0.526 ms against a 7.052 ms full rebuild (held-in).
- **Memory staleness works.** Warm runs selected the cold run's record 12
  times: 4 held-in tasks that read files x 3 trials. In drift, `config_edit`
  invalidated `t04`'s record 3 times (it was derived from the changed config
  file), so it was not selected. Held-out tasks never selected a memory item.
- **Context has a token cost.** Estimated input rises from a held-in median of
  1050.5 (baseline) to 2660.5-2855.5 (cold/warm/drift). That is the retrieved
  index and memory context. With a scripted model nothing offsets it, and this
  run claims no saving.

## What this run does not show

The JSON report's `limitations` array applies in full. In short:

- It does not show paid-model quality, accepted engineering throughput, or any
  token or cost saving. Reported usage is unknown everywhere.
- Wall times are sub-millisecond to a few milliseconds on a tiny fixture, from
  sequential runs on one machine. They are mechanics, not a performance claim.
- CPU time, resident memory, interventions and maintenance cost were not
  measured.
