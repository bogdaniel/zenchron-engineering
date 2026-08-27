# Architectural Principles

These principles define the initial architecture. Changes require an explicit ADR rather than silent drift.

## P1 — Compile engineering obligations, not agent workflows

Describe what must be established, not a permanent sequence of AI job titles. Execution planning is replaceable and may change as models improve.

## P2 — Execution is not authority

Agents may reason, plan, modify code, invoke tools, and propose remediation. A producer of a material change may not unilaterally authorize that outcome.

## P3 — Typed facts drive policy

Authorization policy operates on explicit engineering facts and hazards, not an opaque aggregate risk score.

## P4 — Facts carry provenance and uncertainty

A fact must be attributable to its source. Material facts may be `true`, `false`, or `unknown`; confidence and provenance are explicit.

## P5 — Unknowns are resolved or governed conservatively

Unknown material facts first create resolution obligations. If uncertainty remains, assurance requirements may increase; unknown must never silently become false.

## P6 — Capability, permission, and authority are different

A runtime may technically possess a capability that a work contract does not permit. Even a permitted operation requires current authority when it changes protected state.

## P7 — Evidence is exact and contextual

Evidence binds to a subject, repository/work revision, contract revision, producer, environment, and applicable policy. Changed subjects can make prior evidence stale.

## P8 — Material scope expansion triggers reassessment

Observed work is continuously compared with the governance envelope. Material new impact causes fact re-evaluation and contract revision.

## P9 — Obligations may escalate automatically; privilege may not

The system may automatically require more testing, evidence, or review. It may not grant itself new sensitive privileges merely because execution discovered a need.

## P10 — Authority decisions are reproducible

Given the same relevant state, facts, policy revision, contract, and valid evidence, the authority evaluator should reach the same decision.

## P11 — Execution learning and governance evolution are separate

The system may optimize model selection, context, retries, decomposition, or cost. Changes to policy, mandatory evidence, or authority boundaries require separate governance authority.

## P12 — Material acceptance requires independent evidence

The producer of a material change cannot be the sole source of the evidence used to authorize it. Independent deterministic tools, assurance providers, other reviewers, or appropriately authorized humans provide separation.
