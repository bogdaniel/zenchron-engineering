package planning_test

import (
	"reflect"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
)

func TestEveryRoleHasACapabilityFloor(t *testing.T) {
	for _, role := range domain.EngineeringRoles() {
		capabilities := planning.RoleCapabilities(role)
		if len(capabilities) == 0 {
			t.Fatalf("role %q states no capability floor, so nothing could be found ineligible for it", role)
		}
		for _, capability := range capabilities {
			if !domain.KnownCapability(capability) {
				t.Fatalf("role %q requires capability %q, which is not in the v0 ontology", role, capability)
			}
		}
	}
}

func TestCapabilityFloorIsOntologyOrdered(t *testing.T) {
	// Canonical order matters: a floor is compared and merged into stage
	// requirements, and two orderings of the same set would compile to two
	// different documents.
	got := planning.RoleCapabilities(domain.RoleImplementer)
	want := []domain.EngineeringCapability{domain.CapabilityRepositoryAnalysis, domain.CapabilityCodeChange}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("implementer floor = %v, want ontology order %v", got, want)
	}
}

func TestOnlyChangeProducingRolesAreMaterialProducers(t *testing.T) {
	material := map[domain.EngineeringRole]bool{
		domain.RoleImplementer: true, domain.RoleTester: true, domain.RoleIntegrator: true,
	}
	for _, role := range domain.EngineeringRoles() {
		if planning.ProducesMaterialChange(role) != material[role] {
			t.Fatalf("role %q material-producer = %v, want %v", role, planning.ProducesMaterialChange(role), material[role])
		}
	}
}

// The planner role reasons; it never writes. A mutating planner invocation
// would be the hidden autonomous plan replacement #64 refuses.
func TestPlannerRoleRequiresANonMutatingInvocation(t *testing.T) {
	if got := planning.RequiredInvocationMode(domain.RolePlanner); got != domain.InvocationModeNonMutatingPlanning {
		t.Fatalf("planner invocation mode = %q, want %q", got, domain.InvocationModeNonMutatingPlanning)
	}
	for _, role := range domain.EngineeringRoles() {
		if role == domain.RolePlanner {
			continue
		}
		if got := planning.RequiredInvocationMode(role); got != domain.InvocationModeMutating {
			t.Fatalf("role %q invocation mode = %q, want %q", role, got, domain.InvocationModeMutating)
		}
	}
}
