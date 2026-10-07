# Serve leverage benchmark (version 1)

The product question is accepted engineering changes per human supervision
hour with the same operator, agents, tasks and acceptance criteria. Version 1
rebases the earlier #11/#42–#45 measurement concepts on `serve` (#63), superseding
watch-first sequencing. The existing issue-77 record is an immutable historical
observation in version 0.1; it has unknown attention and no baseline and cannot
be used to compute leverage. No live paired benchmark has yet been collected.
**M1's 2× hypothesis remains unproven.** Synthetic tests are not product evidence.

## Running the offline evaluator

Record a version 1 dataset using the JSON fields in `benchmark.Input`,
`benchmark.Cohort` and `benchmark.Task` in `benchmark/benchmark.go`, then run:

```sh
go run ./cmd/zenchron-benchmark < observations.json > report.json
```

`example.synthetic.json` is a runnable input example, not a measured result.
Its thin sample must report an unproven gate even though its invented ratio is 2.
Replace every synthetic identity and observation before using it for collection.

The command reads only stdin, writes only stdout, rejects unknown fields and
invalid/incomparable pairs, and never operates agents, GitHub or authority.
Keep raw observations alongside the report; correct errors in a new version,
never silently replace a disappointing result. The historical raw record stays
unchanged. `schema_version` is `1`, `benchmark` is `zenchron-leverage`, and
`corpus_version` is `serve-v1`. Predeclare `minimum_per_class` (at least 3).
This is a minimum coverage rule, not a statistical confidence guarantee.

## Collection protocol

1. Freeze repository/base revision, controller and Engineering build identity,
   operator, agent/provider/model/trust mode, acceptance criteria and task IDs.
   Use fresh equivalent workspaces. Alternate/randomize mode order across
   repetitions to reduce learning effects; retain failures and abandoned tasks.
2. In `direct_agent`, use the normal convenient agent workflow, including its
   ordinary tools. In `zenchron_explicit`, assign the same work to `serve` workers
   using the documented `serve` workflow. Record `zenchron_planned` separately
   when planning is enabled; it does not substitute for the explicit baseline.
   Include setup, failed attempts and remediation in the observation window.
3. Use the corpus below, adapting concrete task details to the frozen repository
   before either run. Matching cohorts share an ID; task IDs, class and exact
   acceptance text must match. Record all count and boolean fields explicitly.
   `accepted` means independently accepted under those criteria, not merely a
   PR opened or an agent claiming success. Review-ready is not acceptance.
4. Time operator attention as disjoint UTC start/end intervals with categories
   `selection_assignment`, `active_before_review`, `relay`, `workaround`,
   `authority`, and `review`. Charge each minute once. Include planning and
   assignment in the denominator. Concurrent tasks share one cohort attention
   ledger; never add per-task attention estimates. Sequential repetitions must
   have separate observation windows. Unknown attention invalidates a run for
   rate measurement; an empty ledger means actually observed zero attention.
5. Record prompt/log/review-message relay actions, operational workarounds,
   intended authority interventions, review cycles, rework, first-pass acceptance,
   escaped controlled defects, unsafe authority outcomes and false blocks.
   Record authority TP/FP/FN only with adjudicated ground truth, otherwise null.
   Measure unattended provider/CI waiting separately; it is not attention.
6. Record worker start/end times and identities. The concurrent cohort must
   overlap distinct workers, inject one failure, and observe whether unrelated
   work continues. `failure_blocked_unrelated: null` means not observed. Compare
   against sequential manual operation. Record all manual relays even when the
   runtime was expected to handle them. The report shows overlap wall minutes
   and attention during overlap without double counting multiworker overlap.
7. Cost is `{ "amount": null, "currency": "", "reason": "subscription cost not reported" }`
   unless actually reported. Usage tokens are null when unknown. Known zero cost
   requires an actual report. Costs remain per cohort, without mixing currencies.

## Corpus serve-v1

The machine-readable case specifications are in `corpus-v1.json`.
Use at least three paired repetitions per class, including representative real
repository changes. These are case specifications, not prefilled outcomes.

| Class | Work and acceptance contract |
| --- | --- |
| trivial/documentation | Correct a real setup instruction; verify the documented command. |
| normal_behavior | Repair an observable behavior with a regression check. |
| api_business_rule | Change a business rule; check allowed and rejected API inputs. |
| security_sensitive | Repair an authorization check; test permitted and forbidden callers. |
| hidden_scope_expansion | Reveal a material dependency after assignment; review expanded scope before acceptance. |
| failing_tests_remediation | Supply a failing regression; accept only after the exact candidate passes. |
| review_feedback | Give equivalent independent review feedback; verify the correction. |
| provider_wait | Exercise a controlled transient/quota wait; verify recovery and preserve waiting observations. |
| base_drift_conflict | Advance the base with a conflict; require valid resolution and refreshed checks. |
| concurrent_cohort | Run two independent changes on distinct workers; fail one, verify the other progresses and both meet their criteria. |

## Reading results

The primary rate is accepted changes × 60 / measured supervision minutes.
The explicit/direct ratio is emitted unchanged whenever defined, including
values below one. Zero baseline throughput or zero attention produces null,
not infinity. Missing pairs, thin coverage or absent failure-isolation proof
leave the gate `unproven`. A complete sample below 2 is `below_target`.
`meets_observed_target` requires at least 2×, representative class coverage,
concurrent-worker evidence versus a sequential baseline, and conservatively
zero observed escaped defects and unsafe authority outcomes in explicit mode.
It is an observed threshold, not a claim of population-level certainty.
Publish sample size, raw records, case selection and limitations with any claim.
Review whether the chosen real cases are representative before roadmap labeling.

The evaluator changes no engineering fact inference, authorization decision,
credential handling or sensitive-data boundary. Its inputs are operator-supplied
measurement data, never execution or approval instructions.
