package runtime

import "fmt"

// The cross-process source claim.
//
// There is deliberately no claims table and no claim lock. A logical source -
// "issue N of this repository, under this operator configuration, at this
// generation" - is claimed by the runs PRIMARY KEY, because the run identity IS
// the source identity. issueRunID derives it from the repository, the issue
// number, the configuration digest and the generation counter and from nothing
// else: no path, no PID, no clock. Two processes that discover the same source
// therefore compute the same primary key before either touches the database, so
// "two independent EngineeringRuns for one source" is not a race that can be
// lost - it is unrepresentable. Adding a second durable name for the same thing
// would only create a way for the two names to disagree.
//
// What genuinely was a query-then-insert race is the write that follows the
// derivation. StartOrResumeIssueRun reads the identity, finds it absent, and
// creates it; PutRun is an upsert, so the loser of that race overwrote the
// winner's run document - with its own CreatedAt/UpdatedAt - after the winner
// had already hashed the run.created event against the row it wrote. ClaimRun
// closes exactly that: one conditional INSERT, the database decides which
// process created the run, and the loser adopts the winner's row instead of
// replacing it.
//
// Duplicate run.created events were never possible and still are not: the
// genesis event id is derived from the run identity and events.id is a PRIMARY
// KEY, so only the claim winner ever appends one.
//
// ponytail: a crash between the claim and its genesis append leaves a run row
// with an empty journal. It replays as active and resumes correctly, it just
// has no run.created event. Healing it needs the claim and the first append to
// share one transaction, which means reaching into the frozen journal; do that
// only if the missing genesis event ever matters to a reader.

// ClaimRun durably claims a run identity. It reports false when the identity
// already exists, whoever wrote it, and never modifies the stored row - so the
// caller that loses the claim adopts the winner's run rather than overwriting
// it. Updates to an existing run go through PutRun, which is the upsert.
func (s *SQLiteOperationStore) ClaimRun(run EngineeringRun) (bool, error) {
	if run.ID == "" {
		return false, fmt.Errorf("run id is required")
	}
	document, err := CanonicalJSON(run)
	if err != nil {
		return false, err
	}
	result, err := s.db.Exec(`INSERT INTO runs (`+sqliteRunColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		run.ID, run.Repository, run.Base.ID, run.Base.Revision, run.Contract.ID, run.Contract.Revision,
		run.Candidate.Branch, run.Candidate.Revision, run.Candidate.Tree, run.ControllerSHA256,
		run.CreatedAt.UnixNano(), string(document))
	if err != nil {
		return false, err
	}
	claimed, err := result.RowsAffected()
	return claimed == 1, err
}
