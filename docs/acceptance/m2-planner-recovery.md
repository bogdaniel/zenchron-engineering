# M2 dogfood: the planner path, refused twice and then repaired

This is the record of #120: the first combined #63 + #106 dogfood against #119
did not reach an operator-reviewable plan, and this is what actually happened
across three attempts at the same command.

It is a RECORD, not a demonstration. Two of the three attempts failed, and the
second failure was not the one the issue was opened about.

## The command, unchanged throughout

```bash
zenchron-engineering autonomy plan issue 119 --text
```

Baseline configuration, unchanged throughout: `default_agent = codex`,
`planning_dir = null`, `codex` and `claude` registered, no template, no
`--agent`, no `--deterministic`.

Plan identity, unchanged throughout, because it is derived from the repository,
the issue and the operator configuration:

```text
plan-bf8f68e58adc7f91f2a2425922ee2c37
```

## Attempt 1 — the compiler manufactured a cycle

Adopted base `a556efd1fc6b0d05dd7e000d7733263b637fe25b`.

The model proposed an acyclic decomposition:

```text
harden-runtime-transitions -> correct-operator-views -> verify-combined-candidate
```

Both implementation stages also carried `"independence": {"dimension":
"execution_agent", "different_from": []}`. The output contract said the answer
must contain exactly the members shown and showed `independence`; the decoder
treats it as optional; and the compiler read a present-but-empty
`different_from` as "bind to every material producer" and then added those
producers as dependencies. For two producers that is a mutual dependency.

```text
zenchron-engineering: plan plan-bf8f68e5... is invalid: stage dependencies form
a cycle among assurance, correct-operator-views, harden-runtime-transitions,
verify-combined-candidate
```

The validator was right. Nothing was persisted, so `plan list` said `no plans`
and `plan show` said `no such plan`.

## Attempt 2 — the runtime read the prompt instead of the answer

Candidate `2c21962`, carrying the contract repair, the independence rule and the
attempt record.

The model answered **correctly**: no `independence` on either producer, stated
with explicit peers on the reviewer, `required_claims` used on the gate, the
assurance gate left for the contract to complete. The runtime refused it:

```text
zenchron-engineering: planning invocation by agent codex is refused: proposed
stage "kebab-case-id" has kind "agent|assurance_gate|human_decision_gate", which
is not a stage kind
```

`kebab-case-id` is the output contract's own example. `extractJSONObject` scans a
whole transcript for balanced JSON objects, and a coding CLI transcript is mostly
not brace-and-quote structure — it echoes Go source, test names and prose. On
this transcript that left **29 unclosed braces and 16 recorded spans**: the
model's answer never formed a span at all, and the last thing that still parsed
was the contract example the provider had echoed as part of the prompt.

The fix is to read the FENCE the contract already asks for. The brace scan
remains the fallback for a model that answers without one.

This attempt also found the second half of the durability gap: a planner refusal
happens before any proposal exists to compile, so it took an early return and
left nothing behind — the same product gap, one layer up.

## Attempt 3 — an operator-reviewable plan

Candidate `c5c67d1`.

```text
proposed plan plan-bf8f68e58adc7f91f2a2425922ee2c37 revision 1
planned by: codex (codex_cli, openai) in non_mutating_planning mode;
            workspace verified unchanged: true
stages:
  harden-planner-cohort            agent           role=implementer
                                                   profile=codex  agent=codex (openai)
  independent-cohort-verification  agent           role=reviewer
                                                   profile=claude agent=claude (anthropic)
                                                   independent-of=harden-planner-cohort
                                                     in agent_profile
  cohort-assurance                 assurance_gate  references claims claim-validation
budget: child runs 0/6, provider invocations 0/18, concurrency ceiling 2
cost: unknown - no configured provider reported one, and unknown is not zero
nothing executes until it is approved
```

`plan show` renders it. The planner chose two workers from different vendors on
its own, with the reviewer independent of the producer it reviews.

### The referenced issue context it was given

```text
issue #110 pinned at 2c24d5b45170      issue #63  pinned at 9ca0c9326c34
issue #111 pinned at d7de6d215b75      issue #67  pinned at 922776a1f662
issue #112 pinned at a5a827c45fd8      issue #103 pinned at af4a69947683
issue #113 pinned at d8a0d063736f      issue #107 pinned at 19b4c887e058
issue #118 pinned at 36f991e08cbf
issue #106 WAS NOT AVAILABLE to the planner: it is a pull request, not a source issue
```

All five cohort issues reached the planner as pinned `UNTRUSTED-SOURCE` text,
along with the four issues #119 cites in its non-goals. #106 is a pull request
rather than an issue, and the forge boundary refuses it — which is exactly the
case the hydration record exists for: the operator can see it on the plan rather
than reading it in the model's prose.

The planner made no network request. The controller read those issues through
the same governed forge boundary that reads the primary issue, before the
invocation started.

## What did not happen

Nothing was approved. Nothing executed. No run was created, no child run was
consumed, and nothing was merged. The plan is left pending on purpose: it is the
proposal for #110/#111/#112/#113/#118, which is the workload this repair exists
to make plannable, and it is the operator's to approve.

## What this run did not exercise

The durable attempt record. Attempt 1 predates it and attempt 2 failed on the
path that did not yet record one; attempt 3 succeeded, so nothing was refused
after the code existed. Its behaviour — including that it survives a restart
unchanged — is proven by regressions rather than by this run.
