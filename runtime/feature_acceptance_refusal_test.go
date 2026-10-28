package runtime

// Fail-closed cases of the feature-acceptance journey: a declaration nothing
// enforces, and a verifier that passes a candidate with a record that does not
// carry the complete current binding.

import (
	"strings"
	"testing"
)

// A declaration with nothing to enforce it, and a verifier that ignores it, both
// fail closed: the integrator never reaches a generic pass and never hands off.
func TestADeclaredFeatureNothingEnforcesNeverReleasesTheIntegrator(t *testing.T) {
	for name, options := range map[string]featureJourneyOptions{
		"the policy rule was never activated": {withoutRule: true},
		"the verifier ignores the feature":    {ignoreFeature: true},
	} {
		t.Run(name, func(t *testing.T) {
			j := newFeatureJourneyWith(t, &featureJourneyWorker{}, options)
			for range 40 {
				j.tick()
			}
			unit := j.fleet.graph(j.graphID).unit(t, "integrate")
			if unit.Output != nil {
				t.Fatalf("the integrator handed off with no feature acceptance: %+v", unit.Output)
			}
			if unit.RunID == "" {
				t.Fatal("the integrator never started, so nothing was tested")
			}
			state := j.fleet.state(unit.RunID)
			if a := state.projection.Assurance; a != nil && a.Passed {
				t.Fatalf("a passing assurance was recorded for a candidate that never proved its feature: %+v", a)
			}
		})
	}
}

// A verifier that returns Passed with a structurally valid but mismatched
// feature record must not admit an integration handoff: every field of the
// current binding is individually load-bearing.
func TestAMismatchedFeatureRecordNeverAdmitsAnIntegrationHandoff(t *testing.T) {
	other := strings.Repeat("e", 40)
	// The control: the same fixture with an exact record IS admitted, so a refusal
	// below is the mismatch's and nothing else's.
	t.Run("an exact record", func(t *testing.T) {
		j := newFeatureJourneyWith(t, &featureJourneyWorker{alwaysFix: true}, featureJourneyOptions{})
		j.until("the exact record releases the integrator", func() bool {
			return j.fleet.graph(j.graphID).unit(t, "integrate").Output != nil
		})
	})
	for name, mutate := range map[string]func(*VerificationCheck){
		"the wrong base":           func(c *VerificationCheck) { c.BaseRevision = other },
		"a stale checker revision": func(c *VerificationCheck) { c.CheckerRevision = "stale-checker-revision" },
		"another commit":           func(c *VerificationCheck) { c.Commit = other },
		"another tree":             func(c *VerificationCheck) { c.Tree = other },
		"another selection":        func(c *VerificationCheck) { c.Selection = strings.Repeat("9", 64) },
		"a skipped test":           func(c *VerificationCheck) { c.Results = &CheckResults{Total: 2, Passed: 1, Skipped: 1} },
		"fewer tests than the manifest": func(c *VerificationCheck) {
			c.Results = &CheckResults{Total: 1, Passed: 1}
		},
		"another feature":     func(c *VerificationCheck) { c.Feature = "another-feature" },
		"another record kind": func(c *VerificationCheck) { c.Kind, c.Checker = CheckKindBrowserAcceptance, browserCheckerID },
		"no artifacts":        func(c *VerificationCheck) { c.ArtifactsSHA256 = "" },
	} {
		t.Run(name, func(t *testing.T) {
			j := newFeatureJourneyWith(t, &featureJourneyWorker{alwaysFix: true}, featureJourneyOptions{mutate: mutate})
			// The corrected tree is the one that passes, so drive until the
			// integrator has been verified at least once.
			j.until("the integrator was verified", func() bool { return len(j.assurance.featureRequests()) > 0 })
			for range 30 {
				j.tick()
			}
			unit := j.fleet.graph(j.graphID).unit(t, "integrate")
			if unit.Output != nil {
				t.Fatalf("the integrator handed off on %s: %+v", name, unit.Output)
			}
			if a := j.fleet.state(unit.RunID).projection.Assurance; a != nil && a.Passed {
				t.Fatalf("a passing observation was journalled on %s: %+v", name, a)
			}
		})
	}
}
