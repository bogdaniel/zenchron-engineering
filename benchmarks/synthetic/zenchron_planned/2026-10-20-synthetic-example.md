# Benchmark result: synthetic-example (zenchron_planned)

**SYNTHETIC EXAMPLE. Not evidence of anything.**

Validity: valid.

Outcome: accepted (operator_stated). Base 0000000000000000000000000000000000000000. Instrumentation 66-minimal-slice/1. Protocol unfrozen-placeholder.

## Human attention

- Operator: synthetic-operator
- Active human time: 18.0 min (unattended waiting excluded: 42.0 min)
- Authority interventions: 1; final acceptance/merge steps: 1
- Recovery workarounds: 0; manual relays: 0 (self_reported: 0 audited, 0 self-reported)

## Runtime

- Child runs: 2
- Operation attempts: 2
- Review observations: 0; review continuations: 1
- Peak run concurrency: 2; run overlap seconds: 360
- Runtime wall seconds: 840
- Tokens: unknown (no durable per-run token record exists in runtime state)
- Cost: unknown (no plan stream consumption with a reported cost; a subscription CLI reports none, and unknown is not zero)

## Sources

- activity_log: `2026-10-20-synthetic-example.activity.jsonl` sha256 `f6d3ca1b7fb2c3a56601a502aa09b34bb7fd90792906381d298ba5b1bc3bcf77`
- runtime_collection: `2026-10-20-synthetic-example.runtime.json` sha256 `e653b7501dc1812d93b18522e5ee9365d848b5db73eec5437cd4c6d67b62578b`

Content digest: `fbd2ba0a409ac011ad2807417c5f893e5ebdbdfa9ac15a3f66fc7c110843ad32`

## Limitations

- n=1: one paired observation is not a representative result
- order effect: the second arm benefits from the operator having already learned the task; this is not removed by measurement
