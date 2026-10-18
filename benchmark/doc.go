// Package benchmark is the minimal measurement slice of #66: an append-only
// operator activity log, a bounded collector over EXISTING durable runtime
// state, and an immutable result emitter in the benchmarks/ shape.
//
// It is deliberately not a second run database, scheduler or orchestration
// service. It reads the runtime through a read-only port, owns only the
// operator's own activity log, and writes create-exclusive result files.
// Nothing in the authorization kernel depends on it.
package benchmark
