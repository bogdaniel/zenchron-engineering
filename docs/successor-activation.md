# Following trusted main

An adopted controller can supervise automatic successor builds and serve
activation:

```sh
/path/to/adopted/zenchron-engineering serve \
  --config /path/to/operator.json \
  --follow-main /path/to/zenchron-engineering-clone
```

Enable this when starting the service, after stopping an existing standalone
serve. The source clone must identify `bogdaniel/zenchron-engineering`; the
ordinary `--repo` argument still selects engineering work. The operator's
separate governance credential and pinned assurance image/cache are required.
Ordinary `serve` retains its existing behavior.

The original adopted process remains a guardian, with one serving child. Every
minute it observes governed main. An exact revision must descend from the active
controller; discovery alone never authorizes adoption. The guardian calls the
same `BuildAdoptedController` implementation as `controller build-adopted`, with
its trust-root revalidation, pinned isolated build, digest, and self-probe checks.
Repeated observations reuse the immutable artifact after fresh governance and
artifact verification. Failed builds leave the serving child running and retry
at the next bounded poll.

Artifacts and provenance live under `<state_dir>/controllers/successor-<sha>/`.
`controllers/successor-status.json` records the latest observation, active
identity, successor provenance, and compatibility blockers. Raw forge errors and
credentials are not persisted there. `controllers/active.json` separately records
the last activated artifact so subsequent failed observations cannot erase the
restart identity. A guardian restart verifies that artifact before serving it.

Activation currently requires no nonterminal runs or plans. The gate replays run
journals and uses the existing plan lifecycle projection, including attempt-only
plans. Live work is classified `compatible_with_migration` and waits: this
runtime has no automatic cross-controller migration protocol. Unsupported run
schemas fail closed; configuration drift is `waiting_configuration_drift`, and
unverifiable or changed governance is `blocked_governance_change`. An empty
blocker list is compatible. The gate never changes historical controller
identity, run journals, or approved plan documents.

Before stopping the old child, the guardian also probes the successor's
`controller inspect-state-format` response. The protocol, runtime schema, and
digest of the entire SQLite migration sequence must agree. A changed migration
therefore waits for an explicit migration workflow before it can touch live
state, even if its version number was not changed.

For an eligible artifact, the guardian stops and joins the old child, rechecks
durable state and governance, then starts the successor in standby. Standby
accepts identity probes but cannot schedule work or accept submissions. The
guardian verifies both PID and the complete build identity, durably records the
restart identity, then activates the child. Startup or readiness failure restarts
the previous executable. If an activation acknowledgement is lost, the guardian
retries activation on the same successor: rolling back after the successor may
have created work would be unsafe. A shutdown that does not finish prevents a
second child from starting.

There is a brief endpoint outage during handoff; this is not a zero-downtime
socket transfer. The guardian remains alive and forwards shutdown to its child,
so a service manager retains one stable PID. The guardian itself is not replaced
in place. A killed guardian with an orphaned child or stale endpoint requires
operator recovery; startup refuses to compete for that endpoint.

This adds local lifecycle control under the existing owner-only endpoint and an
opt-in adopted-guardian decision path. It grants no merge authority and does not
change publication credentials, repository-local binaries, or PATH installs.
Candidate and unattested guardians cannot authorize automatic adoption.
