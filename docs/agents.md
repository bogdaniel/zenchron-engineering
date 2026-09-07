# Agent readiness

This document is about one distinction the operator surface does not make for
you: readiness is not account health.

There is no `autonomy agents` subcommand in this tree. The command set is
`run`, `status`, `events`, `resume`, `refresh`, `authorize`, `stop`, `watch`,
`doctor`, and `gc`. What follows therefore applies to every surface that pairs
a readiness verdict with an authentication mode - `autonomy doctor`'s provider
group, the `selfhost` preflight, the `auth_mode` echoed onto every execution
result, and any future listing that reports the same two things side by side.

## Two questions, one of them free

An operator asking whether an agent is usable is really asking two questions:

```text
can this machine start the agent?   ->  answerable locally, for free
will the account behind it work?    ->  answerable only by paying for a request
```

Every readiness surface here answers the first. None of them answers the
second, and none of them can: a preflight makes no provider inference call, so
it never learns anything only the provider knows.

## What "ready" is decided by

Readiness is decided by the executable and the capabilities it advertises.
`NativeCodexProvider.probe` requires `codex` to resolve on `PATH` and requires
`codex exec --help` and `codex --help` to advertise `--sandbox`,
`workspace-write`, `--ignore-user-config`, `--cd`, `--config`, and
`--ask-for-approval`; a CLI that does not advertise them is
`ErrSandboxUnavailable`, never a silent fallback to unsandboxed Codex.
`DiagnoseSandbox` reports the result of that same probe as `ProviderSandbox`.
`provider.isolation` reads the adapter's own isolation claim and refuses a
provider that reports a required property as unproven.

Both are answers about *this machine*: a program is installed, it is new
enough, and the adapter claims a boundary. Neither has spoken to the provider.

## What an authentication mode is

Three different things carry an auth mode, and all three are an observed
artifact or an operator declaration:

- `provider.auth_mode` in the operator configuration is a declared string. It
  is carried into the provider adapter and echoed back onto every
  `ExecutionResult`. It is what the operator *said*, not something the runtime
  verified.
- The `selfhost` preflight classifies a local sign-in: `codex login status`
  must succeed, and its text is matched to `chatgpt` or `api`. That observes
  the CLI's local record of a sign-in and the class of credential it names.
- `provider.credential` stats the configured credential path - absolute, an
  existing regular file, mode `0600` - and deliberately never reads it. The
  Codex adapter's equivalent requires `CodexHome` to be an existing
  runtime-owned directory.

## A credential artifact proves existence, not validity

Say it plainly: **a present credential artifact proves that a credential
EXISTS. It does not prove that the session behind it is still valid.**

A file on disk, a runtime-owned home directory, a local login record, a
configured auth mode - each is evidence that someone authenticated here once,
in a particular way. None of them is evidence that the credential still works.
Revocation, expiry, a rotated key, a suspended or unfunded account: every one
of those is a fact held by the provider, and none of them changes the bytes on
this machine. `provider.credential`'s own PASS text says so:

```text
This proves the credential is CONFIGURED, not that the provider account can
execute: account state is only observable by making a paid request, which this
preflight never does
```

That is the deliberate trade. The only probe that could answer the second
question is a real inference request, which costs money and runs a model, and
a readiness check must not do either.

## The consequence, stated as an operator will meet it

During the #63 live acceptance a readiness report showed a Codex worker as
ready, with an auth mode read from a local CLI session whose refresh token had
already been revoked. Both columns were accurate about what they measured. The
very next real invocation failed with `your refresh token was revoked`.

Expect exactly that shape:

- A revoked or expired sign-in still reads **ready**, still reports an auth
  mode, and still passes `provider.credential`. `autonomy doctor` can report
  `FAIL=0` over a dead session.
- The first place the dead session is observable is the first real invocation
  of a run. There is no earlier signal, and its absence is not a defect.

Read the two together as "the agent can be started and a credential of this
class is present", never as "this worker will work".

## Where the run goes when the credential turns out to be dead

What the run does next depends on whether the provider names the failure in a
form the runtime recognizes.

**Recognized account-level refusal.** Today that is exactly one code,
`credit_balance_exhausted` from the OpenAI provider, classified as
`provider_account_unavailable` and routed to `RouteWait`. The attempt is
restored to the scheduler, so waiting burns no budget; the run settles into
`waiting` with the durable reason `execution_provider_account_unavailable`.
No run-failed event is journalled, the base, contract, and pinned-source
bindings are preserved, and the wait is rebuilt from the journal across a
restart. `autonomy status` renders:

```text
next action:           restore execution-provider account availability, then
                       `autonomy resume <run>`
```

The run is waiting for the operator. It did not die, and the fix is outside
the runtime: restore the account, then resume.

**Anything else.** A diagnostic the runtime cannot classify - including a
revoked-token message from the native Codex CLI, which
`ClassifyProviderFailure` matches only against narrow transient-capacity text -
stays `unknown` and stops diagnostically rather than waiting. The runtime does
not invent classifications for provider taxonomies it has not observed. Here
the operator re-authenticates and starts the work again; there is no wait to
resume.

Neither outcome is silent, and neither is prevented by a green readiness
report.

## What this asks of an operator

Treat readiness as necessary and not sufficient. `autonomy doctor` reporting
`FAIL=0` is a statement about this machine's installation and configuration,
and a strong one; it is not a statement about the account. Before a long
unattended run, refresh the sign-in rather than trusting the readiness
verdict, and when a first invocation fails or waits on an authentication
diagnostic, look at the account, not at the runtime.
