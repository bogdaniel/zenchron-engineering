# Benchmark results

#66 defines the leverage benchmark and its three modes:

```text
direct_agent        an operator drives coding agents by hand
zenchron_explicit   an operator assigns tasks through #63 serve
zenchron_planned    Zenchron plans roles and decomposition before execution
```

#66 is **not implemented**. There is no harness here, no corpus and no
comparison: the benchmark program, its supervision-time measurement and the
`direct_agent` / `zenchron_explicit` baselines are that issue's work.

What this directory holds is the raw record of a `zenchron_planned` execution
that actually happened, in the shape #66 asks results to identify themselves
by, so that when the harness exists this result is a data point rather than a
memory. One record per run, immutable once written, named for the date and the
source it answered.

A record states what was measured and marks everything else unknown. In
particular:

- **cost is unknown** where a provider reports none. A subscription CLI reports
  no cost, and rendering that as zero would fabricate a currency figure.
- **supervision minutes** are wall-clock operator attention as observed, not a
  model of it. A single record is a single observation and proves nothing about
  leverage on its own; the ratio #66 gates on needs a corpus and both baselines.

A record must never be edited to look better. If a result is poor, neutral or
invalidated, it says so and says why.
