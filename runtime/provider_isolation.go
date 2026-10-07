package runtime

// Provider isolation is a claim about what has actually been PROVEN, not a
// configuration string. Protected autonomous execution requires that the model
// cannot read runtime state, the controller checkout, other runs' state, or
// provider credentials; a provider that cannot establish one of those
// properties is ineligible rather than silently trusted.
//
// This is deliberately layered on top of execution.Port (execution/port.go) via an
// interface assertion so the provider contract itself stays small and
// provider-independent.

import "fmt"

// IsolationReporter is implemented by providers that state their proven
// boundary. Not implementing it is a valid, and ineligible, answer.
type IsolationReporter interface {
	Isolation() ProviderIsolation
}

// Every native provider states its boundary (#521); losing the method would
// silently make it ineligible, so it fails to compile instead.
var (
	_ IsolationReporter = CLIAgentProvider{}
	_ IsolationReporter = NativeCodexProvider{}
	_ IsolationReporter = OpenAIProvider{}
)

// RequireProtectedIsolation reports whether a provider may be used for
// PROTECTED autonomous execution. It fails closed: a provider that reports no
// isolation at all, or reports any required property as unproven, is
// ineligible and the error names the property.
func RequireProtectedIsolation(provider ExecutionProvider) error {
	reporter, ok := provider.(IsolationReporter)
	if !ok {
		return fmt.Errorf("provider %T is ineligible for protected autonomous execution: it reports no isolation", provider)
	}
	isolation := reporter.Isolation()
	for _, required := range []struct {
		property string
		level    IsolationLevel
	}{
		{"filesystem read confinement", isolation.FilesystemRead},
		{"filesystem write confinement", isolation.FilesystemWrite},
		{"network denial for candidate and tool commands", isolation.NetworkDenied},
		{"provider credential confinement to the control plane", isolation.CredentialScope},
	} {
		if required.level != IsolationProven {
			return fmt.Errorf("provider %T is ineligible for protected autonomous execution: %s is unproven (%s)", provider, required.property, isolation.Rationale)
		}
	}
	return nil
}
