// Package domain defines the durable Zenchron Engineering v0.1 contracts and
// their canonical JSON boundary.
//
// Decode rejects duplicate object member names and trailing content before
// validating input against the merged JSON Schemas. Encode validates the
// resulting representation against the same schemas. Durable contract ingress
// and egress must use these functions rather than encoding/json directly.
//
// encoding/json emits map keys in sorted order, so Encode is deterministic for
// the v0.1 types. Its bytes are not a cryptographic canonicalization format:
// consumers must not use them as stable signatures or cross-implementation
// hashes until Zenchron defines separate canonical hashing rules.
package domain
