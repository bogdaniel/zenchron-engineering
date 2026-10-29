package runtime

import (
	"errors"
	"fmt"
	"strings"
)

// RepositoryTargetError refuses target discovery without a checkout and origin.
// Cause contains only a fixed diagnostic, never Git's potentially sensitive stderr.
type RepositoryTargetError struct {
	Cause         error
	MissingOrigin bool
}

func (e *RepositoryTargetError) Error() string {
	action := "cd into the target repository's checkout"
	if e.MissingOrigin {
		action = "add an origin remote for the target repository"
	}
	return fmt.Sprintf("repository target: this command must run from a Git checkout containing the target repository, with an origin remote; %s (git remote get-url origin: %v)", action, e.Cause)
}

func (e *RepositoryTargetError) Unwrap() error { return e.Cause }

// repositoryTargetFailure is the error ResolveRepository returns when Git could
// not name an origin. The two operator-fixable causes become a typed refusal
// with one next step; anything else keeps its original cause.
func repositoryTargetFailure(err error) error {
	var commandError *gitCommandError
	if errors.As(err, &commandError) {
		switch {
		case strings.Contains(commandError.message, "fatal: not a git repository"):
			return &RepositoryTargetError{Cause: errors.New("not a git repository")}
		case strings.Contains(commandError.message, "error: No such remote 'origin'"):
			return &RepositoryTargetError{Cause: errors.New("no origin remote"), MissingOrigin: true}
		}
	}
	return fmt.Errorf("repository target: %w", err)
}
