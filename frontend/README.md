# Engineering OS frontend

React/TypeScript implements the accepted industrial composition at `/app`, including the four main
experiences, Runs/history, WorkGraphs, Products, agent observations, controller diagnostics and login.
Shared tokens and component CSS live in `controlplane/assets/design/`. `/design` is the frozen fixture
checkpoint. Legacy Go pages remain until verified parity and operator acceptance.

```sh
npm ci --ignore-scripts
npm run build
```

Then build Go from the repository root. The build writes checked-in generated
`controlplane/assets/design/react.js`; Go embeds the bundle and CSS. Production needs only the Go binary.
Commit bundle updates with source; rebuild to verify deterministic output. There is no Vite, router,
state framework, production frontend listener or new browser test framework.

`api.ts` owns the sole bearer transport and shared validation; `operations-api.ts` decodes the existing
sanitized operational contracts. `main.tsx` owns shell/navigation and canceled observations. Shared
components, history and factual inspectors compose the pages. Decisions and stop own only local bound
confirmation/reconciliation, while the Go controller owns eligibility, authority and durable effects.

Visible views reobserve every 20 seconds; hidden tabs and open confirmations suspend observation.
Stale/error snapshots disable review. Token login verifies the existing credential before setting the
existing cookie, with no token in URL/local storage. Runtime never imports fixture measurements.

The reconciliation consumes main's existing objective intake, plan preview/revision/approval,
typed decision resolution, publication human evidence and run-control APIs. `workspace-api.ts`
uses the sole transport; `governed-action.tsx` shares exact-subject confirmation and receipt handling.
No frontend state grants authority. Publication authorization records evidence and does not publish
or merge. Both implementations remain until parity, independent review and operator acceptance.
See [runtime scope/proof](../docs/design/runtime-integration.md),
[batch review](../docs/design/batch-02-review.md) and [retirement inventory](../docs/design/frontend-inventory.md).
