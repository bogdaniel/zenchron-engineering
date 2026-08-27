# JSON Schemas

`schemas/` contains the canonical JSON Schema Draft 2020-12 contracts for
Zenchron Engineering v0.1.

- `project-model.schema.json`
- `engineering-fact.schema.json`
- `engineering-policy.schema.json`
- `engineering-work-contract.schema.json`
- `evidence-bundle.schema.json`
- `authority-decision.schema.json`

Every schema has a stable `$id`, requires `schema_version: "0.1"`, and rejects
undeclared fields. `fixtures/v0.1/` contains the tested contracts. Run
`go test ./...` to compile every schema and validate every fixture.
