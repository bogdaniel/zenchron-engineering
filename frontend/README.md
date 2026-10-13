# Engineering OS frontend — batch 01

React/TypeScript implements the accepted industrial composition at `/app`. Shared tokens,
layout and component CSS remain in `controlplane/assets/design/`; the fixture-only `/design`
checkpoint uses those same styles. There is one runtime frontend entry, one bundle, and no
frontend production listener. Legacy Go pages remain until behavioral parity and operator acceptance.

Build from this directory:

```sh
npm ci --ignore-scripts
npm run build
```

Then run `go build ./...` from the repository root. `build.mjs` writes the checked-in generated
`controlplane/assets/design/react.js`; Go embeds it alongside the shared CSS. Production needs only
the Go binary. Commit bundle updates with their source and lockfile. Rebuild and use `git diff`
to verify the generated asset is current. There is no Vite, router, client domain store, icon
dependency or new browser test framework. React, React DOM, TypeScript, their types and esbuild
are the build dependencies required by the requested React/TypeScript implementation.

`src/api.ts` owns bearer transport, sanitized DTO validation and the sole `plan-reject` submission.
`main.tsx` owns navigation, theme, canceled reads and historical snapshot handling.
`components.tsx` owns reusable presentation and native disclosures/dialogs. `pages.tsx` composes
the read surfaces; `decisions.tsx` owns local confirmation and reconciliation. None owns workflow
authority. The server independently verifies the immutable subject, pending decision, current
controller generation and held role before any journal write.

The live workflow is deliberately **rejection only**. Approval requires unavailable assignment
bindings; publication and merge are unsupported. Missing aggregate metrics, WorkGraph association,
complete activity and installed-agent capacity stay unavailable rather than inferred.
See [runtime scope and proof](../docs/design/runtime-integration.md) and the
[legacy retirement inventory](../docs/design/frontend-inventory.md).
