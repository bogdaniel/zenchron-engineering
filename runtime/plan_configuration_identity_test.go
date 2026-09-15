package runtime

import (
	"context"
	"reflect"
	"testing"
)

// Characterize #131 without choosing a new identity or migration policy.
func TestConfigurationForksPlanLedgerInsteadOfRebinding(t *testing.T) {
	for _, change := range []string{"unchanged", "global", "repository"} {
		t.Run(change, func(t *testing.T) {
			f := newAttemptFixture(t)
			r := &EngineeringRuntime{}
			r.deps.Repository.Identity = "acme/repo"
			r.deps.ConfigDigest = ConfigDigest{Global: "global-before", Repository: "repo-before"}
			beforeID, err := r.PlanID(119)
			if err != nil {
				t.Fatal(err)
			}
			first, err := f.service.Propose(context.Background(), rebindProposal(beforeID, "acme/repo", baseA, baseA))
			if err != nil {
				t.Fatal(err)
			}
			if err := appendPlanEvent(f.store, f.clock.Now(), first.ID, EventPlanBudgetConsumed,
				PlanBudgetConsumedPayload{StageID: "harden-runtime", RunID: "run-before", ChildRuns: 2, ProviderInvocations: 2, WallSeconds: 523}); err != nil {
				t.Fatal(err)
			}
			before, err := f.store.ReplayPlan(beforeID)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "global":
				r.deps.ConfigDigest.Global = "global-after"
			case "repository":
				r.deps.ConfigDigest.Repository = "repo-after"
			}
			afterID, err := r.PlanID(119)
			if err != nil {
				t.Fatal(err)
			}
			next, err := f.service.Propose(context.Background(), rebindProposal(afterID, "acme/repo", baseB, baseB))
			if err != nil {
				t.Fatal(err)
			}
			after, err := f.store.ReplayPlan(afterID)
			if err != nil {
				t.Fatal(err)
			}
			view, err := f.service.ViewRevision(afterID, next.Revision)
			if err != nil {
				t.Fatal(err)
			}
			if change == "unchanged" {
				if afterID != beforeID || next.Revision != first.Revision+1 || view.BaseChange == nil {
					t.Fatal("unchanged configuration must revise and rebind the existing identity")
				}
				if !reflect.DeepEqual(before.Consumed, after.Consumed) {
					t.Fatal("rebinding changed consumption")
				}
				return
			}
			if afterID == beforeID || next.Revision != 1 || view.BaseChange != nil {
				t.Fatal("changed configuration should characterize a separate identity, not a rebind")
			}
			if after.Consumed.ChildRuns != 0 || after.Consumed.ProviderInvocations != 0 || after.Consumed.WallSeconds != 0 {
				t.Fatalf("new identity did not start a fresh ledger: %#v", after.Consumed)
			}
			preserved, err := f.store.ReplayPlan(beforeID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, preserved) {
				t.Fatal("configuration fork modified the prior plan journal")
			}
		})
	}
}
