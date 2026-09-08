# M2 dogfood: Zenchron planning and executing a Zenchron change

This is the record of #64's bounded self-building acceptance: one real Zenchron
issue, planned by a registered reasoning agent, executed through ordinary #63
runs under an operator-approved plan, with an independent review by a different
vendor.

It is a RECORD, not a demonstration. What went wrong is here too, because what
went wrong is most of what the exercise was for.

## Setup

The operator layer, in an operator-owned directory, with no core code change:

```text
~/.zenchron-m2/config.json          state_dir, planning_dir, plan budget ceiling
~/.zenchron-m2/planning/
  instructions/zenchron-implementation.json   how this repository is changed
  instructions/zenchron-review.json           how a change to it is reviewed
  context/independent-review.json             excludes producer_reasoning
  profiles/zenchron-builder.json              -> codex   (openai)
  profiles/zenchron-reviewer.json             -> claude  (anthropic)
  templates/zenchron-docs-change.json         implement -> review -> assurance
```

Registered workers are the two the operator already had: `codex` (Codex CLI)
and `claude` (Claude Code), both `operator_trusted`.

## The source

[#79 — docs: state that `autonomy agents` reports readiness, not account
health](https://github.com/bogdaniel/zenchron-engineering/issues/79). A real,
small, documentation-only issue chosen so the exercise measured the PLANNING
path rather than the difficulty of the change.

## What happened

PLACEHOLDER_TIMELINE

## What it proves

PLACEHOLDER_PROOF

## What it found

PLACEHOLDER_FINDINGS

## Supervision

PLACEHOLDER_SUPERVISION
