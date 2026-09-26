package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type doctorGovernanceObserver struct {
	rulesets []TrustedMainRuleset
	err      error
}

func (o doctorGovernanceObserver) GovernanceProvenance() CredentialProvenance {
	return CredentialProvenance{Role: CredentialRoleGovernance}
}
func (o doctorGovernanceObserver) Rulesets(context.Context, GitHubRepo) ([]TrustedMainRuleset, error) {
	return o.rulesets, o.err
}

func TestDoctorGovernanceVisibility(t *testing.T) {
	for _, tt := range []struct {
		name     string
		observer ForgeGovernance
		repo     string
		status   DoctorStatus
		reason   string
	}{
		{"missing", nil, "acme/widgets", DoctorFail, "governance_credential_mode"},
		{"resolution or read failure", doctorGovernanceObserver{err: errors.New("secret-token")}, "acme/widgets", DoctorFail, "observation failed"},
		{"no repository", doctorGovernanceObserver{}, "", DoctorWarn, "--repo"},
		{"no rulesets", doctorGovernanceObserver{}, "acme/widgets", DoctorWarn, "unverified"},
		{"undisclosed", doctorGovernanceObserver{rulesets: []TrustedMainRuleset{{BypassActorsKnown: true}, {}}}, "acme/widgets", DoctorFail, "bypass_actors"},
		{"disclosed empty", doctorGovernanceObserver{rulesets: []TrustedMainRuleset{{BypassActorsKnown: true}}}, "acme/widgets", DoctorPass, "disclosed"},
		{"disclosed nonempty", doctorGovernanceObserver{rulesets: []TrustedMainRuleset{{BypassActorsKnown: true, BypassActors: 1}}}, "acme/widgets", DoctorPass, "disclosed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			check := doctorGitHubGovernance(context.Background(), DoctorInput{Governance: tt.observer, Repository: RepositoryTarget{Identity: tt.repo}})
			if check.Status != tt.status || !strings.Contains(check.Reason, tt.reason) {
				t.Fatalf("unexpected check: %+v", check)
			}
			if strings.Contains(check.Reason, "secret-token") {
				t.Fatal("credential leaked")
			}
		})
	}
}

func TestDoctorGovernanceProbesBypassDisclosureOverReadOnlyTransport(t *testing.T) {
	for _, tt := range []struct {
		payload string
		status  DoctorStatus
	}{
		{`{"id":1,"current_user_can_bypass":"never"}`, DoctorFail},
		{`{"id":1,"bypass_actors":null}`, DoctorFail},
		{`{"id":1,"bypass_actors":[]}`, DoctorPass},
	} {
		doer := &fakeGitHubDoer{responses: map[string]string{
			"GET /repos/acme/widgets/rulesets":   `[{"id":1}]`,
			"GET /repos/acme/widgets/rulesets/1": tt.payload,
		}}
		check := doctorGitHubGovernance(context.Background(), DoctorInput{
			Repository: RepositoryTarget{Identity: "acme/widgets"},
			Governance: GitHubGovernanceObserver{HTTP: doer, Credential: testGovernanceCredential("governance-secret")},
		})
		if check.Status != tt.status {
			t.Fatalf("%s: %+v", tt.payload, check)
		}
		if len(doer.requests) != 2 {
			t.Fatalf("expected listing and detail reads: %d", len(doer.requests))
		}
		for _, req := range doer.requests {
			if req.Method != "GET" || req.Body != "" || req.Header.Get("Authorization") != "Bearer governance-secret" {
				t.Fatalf("unexpected governance request: %s", req.Method)
			}
		}
		if strings.Contains(check.Reason, "governance-secret") {
			t.Fatal("credential leaked")
		}
	}
}
