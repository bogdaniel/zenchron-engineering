package runtime

// Provider isolation is a claim about what has actually been PROVEN, not a
// configuration string. Protected autonomous execution requires that the model
// cannot read runtime state, the controller checkout, other runs' state, or
// provider credentials; a provider that cannot establish one of those
// properties is ineligible rather than silently trusted.
//
// This is deliberately layered on top of ExecutionProvider (adapters.go) via an
// interface assertion so the provider contract itself stays small and
// provider-independent.

import "fmt"

// IsolationLevel is fail-closed by construction: the zero value is
// IsolationUnproven, so a property nobody filled in never reads as proven.
type IsolationLevel string

const (
	IsolationUnproven IsolationLevel = ""
	IsolationProven   IsolationLevel = "proven"
)

// ProviderIsolation is what a provider claims it can prove about the boundary
// around the tool execution it drives.
type ProviderIsolation struct {
	// FilesystemRead is confinement of READS to the candidate workspace.
	FilesystemRead IsolationLevel
	// FilesystemWrite is confinement of WRITES to the candidate workspace.
	FilesystemWrite IsolationLevel
	// NetworkDenied is denial of network access to candidate/tool commands.
	NetworkDenied IsolationLevel
	// CredentialScope is confinement of provider credentials to the control
	// plane, so no tool command can observe them.
	CredentialScope IsolationLevel
	// Rationale records why an unproven property cannot be proven here.
	Rationale string
}

// IsolationReporter is implemented by providers that state their proven
// boundary. Not implementing it is a valid, and ineligible, answer.
type IsolationReporter interface {
	Isolation() ProviderIsolation
}

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
