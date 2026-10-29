package runtime

import (
	"context"
	"fmt"
)

// OperatorAuthorityRequest resolves the engine through the run's own durable
// repository and worker binding, exactly as the ordinary supervisor tick does.
func (s *Supervisor) OperatorAuthorityRequest(runID string) (*AuthorityRequest, error) {
	run, found, err := s.deps.Store.Run(runID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("run %s was not found", runID)
	}
	engine, err := s.engine(run.Repository, run.AgentID)
	if err != nil {
		return nil, err
	}
	return engine.PendingAuthorityRequest(runID)
}

// RecordOperatorAuthority records evidence only. Authorize independently
// rechecks the exact candidate, contract and current authority request; partial
// objective candidates return no request through objectivePublicationDeferred.
func (s *Supervisor) RecordOperatorAuthority(ctx context.Context, input AuthorizeInput) (AuthorizeResult, error) {
	run, found, err := s.deps.Store.Run(input.RunID)
	if err != nil {
		return AuthorizeResult{}, err
	}
	if !found {
		return AuthorizeResult{}, fmt.Errorf("run %s was not found", input.RunID)
	}
	engine, err := s.engine(run.Repository, run.AgentID)
	if err != nil {
		return AuthorizeResult{}, err
	}
	return engine.Authorize(ctx, input)
}

// GovernedRepositories reports this supervisor's enrolled set, including an
// explicit serve repository that may differ from watch enrolment.
func (s *Supervisor) GovernedRepositories() []GitHubRepo {
	return append([]GitHubRepo(nil), s.deps.Repositories...)
}
