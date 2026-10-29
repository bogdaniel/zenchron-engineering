package execution

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
