# Benchmark result: synthetic-example (direct_agent)

**SYNTHETIC EXAMPLE. Not evidence of anything.**

Validity: valid.

Outcome: accepted (operator_stated). Base 0000000000000000000000000000000000000000. Instrumentation 66-minimal-slice/1. Protocol unfrozen-placeholder.

## Human attention

- Operator: synthetic-operator
- Active human time: 41.0 min (unattended waiting excluded: 27.0 min)
- Authority interventions: 0; final acceptance/merge steps: 1
- Recovery workarounds: 1; manual relays: 2 (self_reported: 0 audited, 2 self-reported)

## Runtime

- Child runs: unknown (no runtime evidence for this arm)
- Execution attempts (worker invocations): unknown (no runtime evidence for this arm); operation attempts, all kinds: unknown (no runtime evidence for this arm)
- Review observations: unknown (no runtime evidence for this arm); review continuations: unknown (no runtime evidence for this arm)
- Peak execution concurrency: unknown (no runtime evidence for this arm); execution overlap seconds: unknown (no runtime evidence for this arm)
- Runtime wall seconds: unknown (no runtime evidence for this arm)
- Tokens: unknown (direct_agent arm: provider usage is not instrumented)
- Cost: unknown (direct_agent arm: provider cost is not instrumented; unknown is not zero)

## Sources

- activity_log: `2026-10-20-synthetic-example.activity.jsonl` sha256 `047efbcafa5d165edfe3b95806a414fa57d509136b5940953a074dad8257b120`

Content digest: `ab8c9072db378a09d1b297121a78a9ed903faa26a08d9f2c887195189f0dd139`

## Limitations

- n=1: one paired observation is not a representative result
- order effect: the second arm benefits from the operator having already learned the task; this is not removed by measurement
