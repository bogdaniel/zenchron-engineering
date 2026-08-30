package runtime

import (
	"context"
	"strings"
	"testing"
)

// declaredIsolationProvider is a provider that reports whatever boundary the
// test wants, so the guard's RULE is exercised and not only its refusals.
type declaredIsolationProvider struct{ isolation ProviderIsolation }

func (declaredIsolationProvider) Execute(context.Context, ExecutionRequest) (ExecutionResult, error) {
	return ExecutionResult{}, nil
}
func (p declaredIsolationProvider) Isolation() ProviderIsolation { return p.isolation }

// silentProvider reports nothing at all, which must never read as proven.
type silentProvider struct{}

func (silentProvider) Execute(context.Context, ExecutionRequest) (ExecutionResult, error) {
	return ExecutionResult{}, nil
}

func TestProtectedIsolationRejectsNativeCodexForUnprovenFilesystemRead(t *testing.T) {
	err := RequireProtectedIsolation(NativeCodexProvider{})
	if err == nil {
		t.Fatal("NativeCodexProvider was accepted for protected autonomous execution; Codex workspace-write bounds writes, it does not prove read confinement")
	}
	if !strings.Contains(err.Error(), "filesystem read confinement is unproven") {
		t.Fatalf("guard did not name the unproven property: %v", err)
	}
}

func TestProtectedIsolationFailsClosedWithoutAnyIsolationReport(t *testing.T) {
	err := RequireProtectedIsolation(silentProvider{})
	if err == nil {
		t.Fatal("a provider that reports no isolation was treated as proven")
	}
	if !strings.Contains(err.Error(), "reports no isolation") {
		t.Fatalf("guard did not explain the fail-closed refusal: %v", err)
	}
}

func TestProtectedIsolationAcceptsFullyProvenProvider(t *testing.T) {
	proven := ProviderIsolation{FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven, NetworkDenied: IsolationProven, CredentialScope: IsolationProven}
	if err := RequireProtectedIsolation(declaredIsolationProvider{isolation: proven}); err != nil {
		t.Fatalf("a fully proven provider was refused: %v", err)
	}
	// Each required property alone is enough to make the provider ineligible.
	for name, weaken := range map[string]func(*ProviderIsolation){
		"filesystem read confinement":                          func(i *ProviderIsolation) { i.FilesystemRead = IsolationUnproven },
		"filesystem write confinement":                         func(i *ProviderIsolation) { i.FilesystemWrite = IsolationUnproven },
		"network denial for candidate and tool commands":       func(i *ProviderIsolation) { i.NetworkDenied = IsolationUnproven },
		"provider credential confinement to the control plane": func(i *ProviderIsolation) { i.CredentialScope = IsolationUnproven },
	} {
		t.Run(name, func(t *testing.T) {
			weakened := proven
			weaken(&weakened)
			err := RequireProtectedIsolation(declaredIsolationProvider{isolation: weakened})
			if err == nil || !strings.Contains(err.Error(), name+" is unproven") {
				t.Fatalf("guard did not refuse on %s: %v", name, err)
			}
		})
	}
}
