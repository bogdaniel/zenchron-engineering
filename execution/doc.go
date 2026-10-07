// Package execution is the host's one execution seam (ADR-0005, #521): the
// Port an execution implementation satisfies and the vocabulary that crosses
// it. The host (package runtime) owns run and attempt identity, budgets,
// cancellation provenance, the process termination owner, journal and evidence
// recording, candidate authority and durable waits; an implementation owns one
// bounded model/tool execution and its usage and progress observations.
//
// It imports only the standard library and domain. Adapters live under
// execution/<name> and only cmd/ imports them; architecture_test.go enforces
// both.
package execution
