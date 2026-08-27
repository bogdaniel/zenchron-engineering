# v0.1 fixtures

Files under `valid/` are canonical examples. Their names end with the schema
name used to validate them. Files under `invalid/` are targeted contract
violations and must fail validation.

The fixture set covers:

1. trivial work: `trivial.*`;
2. normal behavioral work: `normal-behavior.*`;
3. security-sensitive work: `security-sensitive.*`;
4. hidden scope expansion: `hidden-scope-*`;
5. material uncertainty: `unknown-fact.*`;
6. stale evidence: `stale-evidence.*`;
7. permission expansion: `permission-expansion.*`;
8. failed assurance and remediation: `failed-assurance.*` and `remediated.*`.

These are schema fixtures, not evaluator golden outputs. Cross-object policy,
staleness, and authority semantics belong to later kernel issues.

Identity-addressed definitions use JSON objects keyed by identifier. Targeted
invalid fixtures retain the old duplicate-ID array shape to prove it is rejected.
Each invalid fixture also declares an expected instance location and validation
keyword in `schemas/schema_test.go`; failure for an unrelated reason is not enough.
