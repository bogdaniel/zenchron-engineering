package runtime

// WHICH REVISION ON MAIN IS TRUSTED (ADR-0007).
//
// main_head is an integration fact. trusted_main is an authorization
// projection over exact-revision evidence. Neither the branch position nor a
// green CI check is trust on its own.
//
// The branch-integrity proof (trust_root.go) establishes that refs/heads/main
// only ever moved through merge-commit pull requests with no bypass. This file
// decides which point on that history adoption may stand on: the newest commit
// on main's FIRST-PARENT chain whose own T2 evidence - the full repository
// suite, run by a pinned producer against exactly that commit - is accepted.
//
// Nothing here is stored as "trusted". The answer is recomputed from forge
// observations whenever it is needed, and recorded in the provenance of
// whatever is adopted from it. CI produces evidence; this policy decides what
// the evidence authorizes.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// TrustedRevisionPolicy is the frozen policy for which evidence makes a
// revision trusted. Every field is part of the recorded provenance, so
// changing the bound or the producer changes what an adopted build says it
// was adopted under.
type TrustedRevisionPolicy struct {
	Tier string `json:"tier"`
	// IntegrationID, Workflow, Event, Branch and Job pin the producer. A job
	// named "go" from another app, another workflow, a pull-request event or
	// another branch is not T2 evidence about a main commit, whatever its
	// conclusion.
	IntegrationID int64  `json:"integration_id"`
	Workflow      string `json:"workflow"`
	Event         string `json:"event"`
	Branch        string `json:"branch"`
	Job           string `json:"job"`
	// MaxFirstParent bounds the walk from main_head. 256 covered more than
	// two weeks of merges at the rate observed when ADR-0007 was accepted.
	MaxFirstParent int `json:"max_first_parent"`
}

// githubActionsIntegrationID is the GitHub Actions app: the only producer
// whose jobs are T2 evidence.
const githubActionsIntegrationID = 15368

// DefaultTrustedRevisionPolicy is the policy ADR-0007 accepted.
func DefaultTrustedRevisionPolicy() TrustedRevisionPolicy {
	return TrustedRevisionPolicy{
		Tier: "T2", IntegrationID: githubActionsIntegrationID, Workflow: ".github/workflows/ci.yml", Event: "push", Branch: "main", Job: "go",
		MaxFirstParent: 256,
	}
}

// T2Attempt is one attempt of one producer job, exactly as the forge reported
// it. It is a fact about a run, not yet a judgement about a revision.
type T2Attempt struct {
	RunID   int64 `json:"run_id"`
	Attempt int   `json:"attempt"`
	// IntegrationID is the app that produced the job, as its check run
	// reports it. Zero means the forge did not say, which is not evidence.
	IntegrationID int64     `json:"integration_id"`
	Workflow      string    `json:"workflow"`
	Event         string    `json:"event"`
	Branch        string    `json:"branch"`
	HeadSHA       string    `json:"head_sha"`
	Job           string    `json:"job"`
	Status        string    `json:"status"`
	Conclusion    string    `json:"conclusion,omitempty"`
	CompletedAt   time.Time `json:"completed_at,omitempty"`
}

func (a T2Attempt) completed() bool { return a.Status == "completed" }

// T2EvidenceObservation is the policy's reading of every pinned attempt about
// one exact revision.
type T2EvidenceObservation struct {
	Subject string `json:"subject"`
	Tier    string `json:"tier"`
	// Attempts are every pinned attempt for the subject, completed or not, in
	// (run, attempt) order. Nothing is dropped because a later one disagreed.
	Attempts []T2Attempt `json:"attempts"`
	// Deciding is the latest COMPLETED pinned attempt, which alone decides
	// eligibility. A pending re-run overrides nothing.
	Deciding *T2Attempt `json:"deciding,omitempty"`
	Eligible bool       `json:"eligible"`
	// Inconsistent marks completed attempts that disagree. It never changes
	// eligibility: it is how flakiness stays visible after a later green.
	Inconsistent bool                 `json:"inconsistent,omitempty"`
	ObservedAt   time.Time            `json:"observed_at"`
	ObservedBy   CredentialProvenance `json:"observed_by"`
}

// EvaluateT2Evidence applies the policy to everything the forge reported about
// one revision. It is pure.
func EvaluateT2Evidence(policy TrustedRevisionPolicy, subject string, reported []T2Attempt) T2EvidenceObservation {
	observation := T2EvidenceObservation{Subject: subject, Tier: policy.Tier, Attempts: []T2Attempt{}}
	for _, a := range reported {
		if a.HeadSHA == subject && a.IntegrationID == policy.IntegrationID && a.Workflow == policy.Workflow && a.Event == policy.Event &&
			a.Branch == policy.Branch && a.Job == policy.Job {
			observation.Attempts = append(observation.Attempts, a)
		}
	}
	sort.SliceStable(observation.Attempts, func(i, j int) bool {
		a, b := observation.Attempts[i], observation.Attempts[j]
		return a.RunID < b.RunID || (a.RunID == b.RunID && a.Attempt < b.Attempt)
	})
	var passed, failed bool
	for i := range observation.Attempts {
		a := observation.Attempts[i]
		if !a.completed() {
			continue
		}
		observation.Deciding = &observation.Attempts[i]
		if a.Conclusion == "success" {
			passed = true
		} else {
			failed = true
		}
	}
	observation.Eligible = observation.Deciding != nil && observation.Deciding.Conclusion == "success"
	observation.Inconsistent = passed && failed
	return observation
}

// skipReason says why an ineligible revision was not trusted.
func (o T2EvidenceObservation) skipReason() string {
	switch {
	case len(o.Attempts) == 0:
		return "no pinned T2 evidence"
	case o.Deciding == nil:
		return "T2 pending"
	default:
		return fmt.Sprintf("T2 %s (run %d attempt %d)", o.Deciding.Conclusion, o.Deciding.RunID, o.Deciding.Attempt)
	}
}

// SkippedRevision is a first-parent commit newer than trusted_main, and why it
// was not trusted.
type SkippedRevision struct {
	Revision string `json:"revision"`
	Reason   string `json:"reason"`
}

// TrustedMainResolution is the whole answer: where main is, which revision on
// it is trusted, the evidence that makes it so, and why every newer commit was
// not.
type TrustedMainResolution struct {
	MainHead    string                `json:"main_head"`
	TrustedMain string                `json:"trusted_main"`
	Evidence    T2EvidenceObservation `json:"evidence"`
	Skipped     []SkippedRevision     `json:"skipped"`
}

// ErrNoTrustedMain is no accepted revision within the bound. It never falls
// back to main_head.
var ErrNoTrustedMain = errors.New("no revision on main's first-parent chain has accepted T2 evidence within the bound")

// TrustedMainLineage is the two observations the resolver needs: main's own
// first-parent history, newest first and starting at head, and what the forge
// reports about one exact revision.
type TrustedMainLineage struct {
	FirstParent func(head string, max int) ([]string, error)
	Evidence    func(revision string) ([]T2Attempt, error)
	ObservedBy  CredentialProvenance
	Now         func() time.Time
}

// ResolveTrustedMain walks main's first-parent chain from mainHead and returns
// the newest revision whose exact-revision T2 evidence is accepted.
func ResolveTrustedMain(policy TrustedRevisionPolicy, mainHead string, lineage TrustedMainLineage) (TrustedMainResolution, error) {
	resolution := TrustedMainResolution{MainHead: mainHead, Skipped: []SkippedRevision{}}
	chain, err := lineage.FirstParent(mainHead, policy.MaxFirstParent)
	if err != nil {
		return resolution, fmt.Errorf("main's first-parent history could not be read: %w", err)
	}
	if len(chain) == 0 || chain[0] != mainHead {
		return resolution, fmt.Errorf("main's first-parent history does not start at main_head %s", shortSHA(mainHead))
	}
	if len(chain) > policy.MaxFirstParent {
		chain = chain[:policy.MaxFirstParent]
	}
	for _, revision := range chain {
		reported, err := lineage.Evidence(revision)
		if err != nil {
			return resolution, fmt.Errorf("T2 evidence for %s could not be observed: %w", shortSHA(revision), err)
		}
		observation := EvaluateT2Evidence(policy, revision, reported)
		observation.ObservedAt, observation.ObservedBy = lineage.Now(), lineage.ObservedBy
		if observation.Eligible {
			resolution.TrustedMain, resolution.Evidence = revision, observation
			return resolution, nil
		}
		resolution.Skipped = append(resolution.Skipped, SkippedRevision{Revision: revision, Reason: observation.skipReason()})
	}
	return resolution, fmt.Errorf("%w: %d first-parent commit(s) from %s inspected", ErrNoTrustedMain, len(chain), shortSHA(mainHead))
}

// resolveTrustedMain composes the resolver from the builder's seams: the
// branch is fetched, its first-parent chain read with local Git, and the
// evidence observed by the same governance identity that observed the trust
// root.
func resolveTrustedMain(ctx context.Context, deps AdoptedBuildDeps, repo GitHubRepo, dir, branch string) (TrustedMainResolution, error) {
	mainHead, err := observeMainHead(ctx, deps, repo, branch)
	if err != nil {
		return TrustedMainResolution{}, err
	}
	if err := deps.Fetch(dir, mainHead, branch); err != nil {
		return TrustedMainResolution{}, fmt.Errorf("main_head %s could not be fetched: %w", shortSHA(mainHead), err)
	}
	return ResolveTrustedMain(DefaultTrustedRevisionPolicy(), mainHead, TrustedMainLineage{
		FirstParent: func(head string, max int) ([]string, error) { return firstParentChain(deps.Git, dir, head, max) },
		Evidence: func(revision string) ([]T2Attempt, error) {
			return deps.Governance.RevisionEvidence(ctx, repo, revision)
		},
		ObservedBy: deps.Governance.GovernanceProvenance(),
		Now:        deps.Now,
	})
}

// firstParentChain is main's own history, newest first: on a merge-only branch
// the first-parent chain is exactly the sequence of states the branch held. A
// commit reachable only through a merge's second parent was never main.
func firstParentChain(git func(dir string, args ...string) (string, error), dir, head string, max int) ([]string, error) {
	out, err := git(dir, "rev-list", "--first-parent", fmt.Sprintf("--max-count=%d", max), head)
	return strings.Fields(out), err
}

// isCommitSHA is a full lowercase SHA-1 commit name. Revisions reach a forge
// URL, so nothing else is accepted.
func isCommitSHA(s string) bool {
	if len(s) != 40 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
