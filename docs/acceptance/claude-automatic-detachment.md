# Claude automatic Bash detachment (#388)

The parser correlates main-thread Bash tool-use IDs with user tool-result
blocks, then reads the top-level `tool_use_result.backgroundTaskId` and
`timedOutAfterMs` metadata. Either a nonempty typed background identity or a
positive typed timeout makes the invocation sticky unresolved. Prose, nested
results and other tools cannot trigger this rule. Existing refusal and candidate
quarantine apply equally to explicit and automatic detachment.

## Timeout control verification remains open

The supplied #387 evidence identifies Claude Code 2.1.283 and a 120000 ms
foreground timeout. The workspace contains no implementation or authoritative
control documentation for that version. Its checked-in CLI help fixture is
2.1.282 and does not establish Bash timeout controls. Network and outside-path
access were prohibited during this change. Consequently neither
`BASH_DEFAULT_TIMEOUT_MS` nor `BASH_MAX_TIMEOUT_MS` has been verified here;
this is not evidence that supported controls do not exist. No speculative
control is passed by this change, and the liveness requirement remains open.

Before completing #388, verify both default and maximum timeout semantics
against 2.1.283, including explicit Bash timeout inputs. Derive invocation-local
controls from the effective runtime attempt deadline (including a tighter parent
or stage deadline), reserve bounded time for result delivery, and test that
values remain strictly below the attempt wall. Do not derive them from the
inactivity window: an attached main-thread tool suspends that window.
A fresh live generation with foreground work exceeding 120 seconds is still
required after adoption.

This change does not expand provider permissions or filesystem authorization.
It retains only bounded in-memory tool IDs and a sticky count; background task
IDs, commands and result prose are not added to durable records.
