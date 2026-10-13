# Workflow workspace: isolated acceptance evidence

This is level 2 evidence: Chrome against real isolated SQLite, controller socket,
runtime handlers and HTTP components. Forge/provider boundaries use the existing
test fixtures. It is not a real GitHub objective-to-PR run, independent review,
operator usability acceptance or #448 acceptance. Automated confirmation clicks
are test interactions, not claims that a human witnessed or approved production work.

Base: `f7aaf6a9bed63c54a6716974799f01c262f04ee7`, after #583 merged as
`b461599d64812d9c21b3ea00a8161b1f8d824a62`. The implementation is a stack under
#479/#398, with #508/#474/#475/#480 consumed rather than replaced.

The preserved browser suite passed 25/25, including the original 20 cases and
objective reload, guided exact-preview refusal, typed objective decision
resolution, an incomplete publication outcome and a planning identity without a
proposal. See `browser-results.json` and the desktop/mobile objective screenshots.
Run on a Chrome/Node host:

```sh
ZENCHRON_BROWSER_E2E=1 go test ./cmd/zenchron-engineering -run '^TestBrowserWorkspace$' -count=1 -v
```

Focused Go checks cover explicit graph binding across reopened stores, ambiguity,
bounded plan pages, corrupt/missing revisions, planning-only identities, role-loss
action suppression and unverified plan bindings. Restoring the missing-revision
join defect and the role-loss binding defect each makes its regression fail.
Affected Control Plane/schema/evidence-registry tests, build, vet and file-size
checks passed. The full local module suite was not run; GitHub CI owns that proof.

Remaining acceptance includes multiple producers actually progressing through
the complete browser journey, candidate/finding supersession and lost replies,
remediation/integration history, positive publication, and a real operator-witnessed
GitHub outcome with genuine plan and publication approvals. No production controller
was restarted and no live engineering objective was submitted or published.
