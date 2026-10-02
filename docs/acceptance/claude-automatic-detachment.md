# Claude automatic Bash detachment (#388)

The parser correlates main-thread Bash tool-use IDs with user tool-result
blocks. Automatic detachment requires BOTH a nonempty typed
`tool_use_result.backgroundTaskId` and positive typed `timedOutAfterMs`.
The refusal remains sticky through final answers and process success. Prose,
nested results, unknown IDs and other tools cannot trigger this rule. Explicit
`run_in_background=true` remains a separate dispatch-time refusal path.
Both paths use candidate quarantine and prevent admission of refused mutations.

## Timeout controls and evidence

The admitted owner review for #417 (pull_request_review 5397793007) supplies
direct Claude Code 2.1.283 evidence: `BASH_DEFAULT_TIMEOUT_MS` controls the default
foreground timeout and `BASH_MAX_TIMEOUT_MS` caps an explicit model timeout.
This implementation relies on that admitted evidence; it does not claim a new
local binary verification. A model can still request a shorter explicit timeout.
Typed detachment detection therefore remains authoritative.

At main invocation dispatch, both controls use the remaining effective context
deadline, including a tighter parent or stage deadline. They reserve one
millisecond and round down to milliseconds, remaining strictly within the
attempt wall. Insufficient remaining authority refuses dispatch. Unbounded
callers receive no timeout override. Probes receive neither variable.
The inactivity window is not used: attached main-thread work suspends it.
No repository-specific test duration enters the calculation.

Fresh live acceptance after merge/adoption remains required: foreground work
longer than 120 seconds must stay attached and deliver its actual result.

This change does not expand provider permissions or filesystem authorization.
Background task IDs, commands and result prose are not added to durable records.
