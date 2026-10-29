package runtime

import "strings"

// TypedResultWriter is the capability a worker needs to answer a typed result
// protocol at all: write access to the runtime-owned result directory, which
// is deliberately OUTSIDE the candidate workspace. An orchestrated worker has
// to write its handoff there (#470), so a provider that cannot is refused at
// submission rather than launched into a contract it can never satisfy.
//
// It is optional and fails closed: a provider that does not implement it is
// not capable. Gate (a): CLIAgentProvider, NativeCodexProvider and the test
// workers implement it, and the brokered API provider deliberately does not -
// its tool surface is bound to the candidate workspace.
type TypedResultWriter interface {
	WritesTypedResults() bool
}

var (
	_ TypedResultWriter = CLIAgentProvider{}
	_ TypedResultWriter = NativeCodexProvider{}
)

// writesTypedResults is the one place that question is asked of a provider.
func writesTypedResults(provider ExecutionProvider) bool {
	writer, ok := provider.(TypedResultWriter)
	return ok && writer.WritesTypedResults()
}

// typedResultProbeDir is a sentinel no real invocation uses.
const typedResultProbeDir = "/zenchron-typed-result-capability-probe"

// WritesTypedResults answers from the adapter's OWN argument builder: the
// worker can write the result directory exactly when the argv this adapter
// would run grants it. Nothing here names an agent kind, so an adapter that
// gains or loses the grant changes the answer with it.
func (p CLIAgentProvider) WritesTypedResults() bool {
	spec, err := p.spec()
	if err != nil {
		return false
	}
	args := spec.Args(cliInvocation{CandidateDir: "/candidate", Prompt: "probe", ResultDir: typedResultProbeDir})
	for _, arg := range args {
		if strings.Contains(arg, typedResultProbeDir) {
			return true
		}
	}
	return false
}

// WritesTypedResults is the wrapped CLI adapter's answer.
func (p NativeCodexProvider) WritesTypedResults() bool { return p.provider().WritesTypedResults() }
