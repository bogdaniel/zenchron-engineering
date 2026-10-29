# Reconciliation validation

See [batch review](batch-02-review.md) for results and [inventory](frontend-inventory.md) for parity
and removal gates. Passing simulations never supplies controller or operator acceptance.

## Reproduce on the exact source/bundle

```sh
npm --prefix frontend ci --ignore-scripts
npm --prefix frontend run build
ci/static.sh
go build ./...
go test -race ./controlplane ./schemas -count=1
python3 scripts/check_file_sizes.py --verify-baseline
python3 -m unittest discover -s scripts
```

Run browser checks only on isolated state with already-installed Playwright and Chrome. Supply
`ZENCHRON_PLAYWRIGHT_MODULE`, `ZENCHRON_CHROME_EXECUTABLE`, and artifact directories under temporary
storage. No package install or production controller is required.

```sh
ZENCHRON_REACT_BROWSER_E2E=1 go test ./cmd/zenchron-engineering \
  -run '^TestReactEngineeringOSGovernedWorkspace$' -count=1 -v
ZENCHRON_BROWSER_E2E=1 go test ./cmd/zenchron-engineering \
  -run '^TestBrowserWorkspace$' -count=1 -v
ZENCHRON_REACT_BROWSER_E2E=1 go test ./cmd/zenchron-engineering \
  -run '^TestDashboardDescribedObjective$' -count=1 -v
go test ./cmd/zenchron-engineering \
  -run '^TestEngineeringOSPlanRejectEndToEnd$' -count=1 -v
```

The restart test enables Chrome when `ZENCHRON_PLAYWRIGHT_MODULE` is set. It uses a new temporary
store and real OS-process controller, not an API action fake. Reconciled workflow tests use actual
Go HTTP/control socket/role/journal paths. External forge/model/verifier inputs stay offline.
They record test decisions only, never real publication or production authorization.

Against an isolated Go `control-plane` binary built from the same source:

```sh
ZENCHRON_PREVIEW_TOKEN_FILE=/absolute/path/to/isolated-state/control-plane.token \
ZENCHRON_PREVIEW_SCREENSHOTS=/private/tmp/zenchron-head-preview \
node scripts/check_design_preview.mjs
```

This maintained entry separates frozen fixture checks, actual empty/login captures and HTTP
projection/load fixtures. Runtime never imports fixture measurements. The 1200-unit fixture
asserts first-page readiness within 5 seconds and 50 visible graph nodes/events. Logical objective
and history indexes represent 2000 and 10000 records; their reads are presentation mocks, not SQL
service load qualification. New receipt tests use an actual journal exceeding 500 events.

All 39 schemas compile; relevant Control Plane race checks, strict build/static/Windows checks,
21 Python regressions and size/baseline verification pass. The old head's payload-file overage is
resolved by the integrated main extraction, with no waiver or ceiling increase in this batch.
Full Go suite runs in GitHub CI; only an exact-head green required check qualifies that gate.

## Accessibility and visual evidence limits

Actual rendered checks cover names/roles, contrast, keyboard skip and dialogs/drawer, focus after
resolution, responsive reflow, collapsed mobile bindings and visible decision footers. Chromium
accessibility trees are committed as browser evidence. They do not substitute for manual screen-reader
speech or native browser high-zoom checks. Those and real operator acceptance remain required before
cutover. New runtime screenshots are inspection artifacts; the frozen approved fixture checkpoint
is separately compared byte-for-byte. No new runtime baseline is self-approved.

No obsolete frontend file is removed. Independent re-review and operator acceptance precede
redirects, legacy deletion and the final single-shell production migration.

## Focused lost-response recovery

See [the one-finding follow-up](action-recovery-review.md). The actual-controller browser test now
drops replies for all four affected operations, survives navigation/reload, refuses wrong/missing
receipts and recovers exact results without a second POST. Storage failure sends nothing. Runtime
and HTTP receipt tests validate no workflow-state mutation, durable read-only reopen and tampering.
The old head's full/evidence CI passed; its race job exhausted the package-wide 60-minute budget.
That failure is documented, not waived; new-head CI is reported separately on the PR.
