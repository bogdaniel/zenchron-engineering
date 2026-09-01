package main

// operator.go is the OPERATOR SURFACE of the local engineering runtime:
// authorize, resume, refresh, status, events, doctor and gc.
//
// It obeys the same rule autonomy.go states: build dependencies, call the
// runtime, render output, map exit codes. There is no orchestration here. In
// particular:
//
//   - `authorize` never decides authority. It asks the runtime for the exact
//     pending request, hands the operator's answer to Authorize, and prints
//     what the evaluator concluded afterwards.
//   - `resume` never clears a wait. It asks the runtime to reconcile again;
//     the two waits it refuses to hand over are refused because the runtime's
//     own reconcile cannot re-derive them, and refusing is what preserves
//     their meaning.
//   - `status` derives nothing about what SHOULD happen. Every field is
//     replayed durable state, and the one interpretive field - the next
//     operator action - is a total function of that state, never a summary.
//   - `events` renders the persisted journal in persisted order and prints no
//     artifact content.
//
// If a change here starts describing WHEN something happens rather than WHAT
// is rendered, it belongs in runtime/.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bogdaniel/zenchron-engineering/analysis"
	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// ---------------------------------------------------------------------------
// authorize
// ---------------------------------------------------------------------------

// autonomyAuthorize records ONE human decision against ONE exact request.
//
// The operator types a run id and a request id and nothing else. They never
// type a candidate SHA, a tree, a contract revision, or a controller digest:
// the request id is a digest of all of them, so naming the id IS naming the
// exact state, and the redundant digest and action this command pins are read
// back from the runtime's own projection rather than from the command line.
//
// Nothing here assigns authority. Authorize records evidence; what that
// evidence means is decided by the evaluator, and "recorded, still blocked" and
// "recorded, still incomplete" are ordinary results that this command reports
// as-is. A refusal - the request is stale, the controller changed, the source
// moved, the action is not human-authorizable - exits with its own status.
func autonomyAuthorize(ctx context.Context, engine engineeringRuntime, built *composition, flags autonomyFlags, runID, requestID string, stdout io.Writer) (int, error) {
	if flags.Decision == "" {
		return runtime.ExitInvalid, errors.New("authorize requires exactly one of --approve or --reject")
	}
	if _, err := requireRun(built, runID); err != nil {
		return exitFor(err, runtime.ExitFailed), err
	}
	// Operator identity is resolved BEFORE anything is recorded and fails
	// closed: an empty identity must never flow onward as if it were one.
	operator, err := operatorIdentity(built)
	if err != nil {
		return exitFor(err, runtime.ExitInvalid), err
	}

	// The current request, read from the runtime. It supplies the redundant
	// digest and action pins so the operator does not have to, and a refusal
	// raised while merely READING is already actionable.
	request, err := engine.PendingAuthorityRequest(runID)
	if err != nil {
		return exitFor(err, runtime.ExitFailed), err
	}
	input := runtime.AuthorizeInput{
		RunID:     runID,
		RequestID: requestID,
		Decision:  flags.Decision,
		Note:      flags.Note,
		Operator:  operator,
	}
	if request != nil && request.ID == requestID {
		input.Digest, input.Action = request.Digest, request.Action
	}

	result, err := engine.Authorize(ctx, input)
	if err != nil {
		return exitFor(err, runtime.ExitFailed), err
	}
	if err := writeJSON(stdout, result); err != nil {
		return runtime.ExitFailed, err
	}
	// The reported status is the RE-EVALUATION, so the exit status is the run's
	// own disposition mapping - the same one `run`, `resume` and `status` use.
	return runtime.Outcome{RunID: result.RunID, Disposition: result.Disposition, Reason: result.Reason}.ExitCode(), nil
}

// operatorIdentity resolves recorded operator provenance from the operator
// configuration layer. A CLI driving an injected runtime has no configuration
// layer and falls back to the same fail-closed resolution over an empty one, so
// the local account still has to resolve to something.
func operatorIdentity(built *composition) (runtime.RecordedOperator, error) {
	if built == nil {
		return runtime.OperatorConfig{}.ResolveOperator()
	}
	return built.config.OperatorConfig.ResolveOperator()
}

// ---------------------------------------------------------------------------
// resume
// ---------------------------------------------------------------------------

// autonomyResume means exactly one thing: ask the runtime to reconcile this run
// again. It does NOT mean "clear the waiting reasons".
//
// Every typed wait keeps its own semantics because the runtime re-derives it
// from durable state on the next pass, not because this command inspects it:
//
//	github_auth_required        the credential is read again; a restored one
//	                            simply proceeds, and a missing one waits again
//	awaiting_authority          the authority is evaluated again; evidence that
//	                            now satisfies the claim proceeds
//	controller_changed          conditions() keeps the run waiting, and a
//	                            merged pull request observed on the same pass
//	                            still completes it passively
//	source_intent_changed       conditions() keeps the run waiting. A plain
//	                            resume can therefore NEVER absorb changed
//	                            source intent; `refresh` is the only thing that
//	                            re-reads it, and it is explicit.
//
// Two are refused instead of handed over, because the runtime's reconcile has
// no way to re-derive them and handing them over would be a silent override:
//
//	opt_in_removed              consent was withdrawn. Reconciling would drive
//	                            a run the operator un-enrolled.
//	workspace_integrity         the runtime-owned candidate metadata does not
//	                            match its trusted baseline. There is no silent
//	                            resume over a mismatch.
func autonomyResume(ctx context.Context, engine engineeringRuntime, built *composition, runID string, stdout io.Writer) (int, error) {
	run, err := requireRun(built, runID)
	if err != nil {
		return exitFor(err, runtime.ExitFailed), err
	}
	events, err := engine.Journal(runID)
	if err != nil {
		return exitFor(err, runtime.ExitFailed), err
	}
	if code, detail, refused := resumeRefusal(run, runID, events); refused {
		return code, errors.New(detail)
	}
	return reconcile(ctx, engine, runID, stdout)
}

// resumeRefusal is the complete, ordered list of persisted conditions a plain
// resume must not walk over. It reads durable state only.
func resumeRefusal(run runtime.EngineeringRun, runID string, events []runtime.EngineeringEvent) (int, string, bool) {
	disposition, reason := persistedDisposition(run, events)
	if disposition == runtime.Cancelled {
		// `stop` is explicit operator intent and asking again does not withdraw
		// it; new work for the same issue comes from `run issue <number>`,
		// which already treats a terminal run as a generation boundary.
		return runtime.ExitCancelled, fmt.Sprintf(
			"run %s was cancelled (%s); explicit operator intent is not withdrawn by asking again. Start new work with `autonomy run issue <number>`", runID, reason), true
	}
	if reason == runtime.WatchWaitingOptInRemoved {
		return runtime.ExitWaiting, fmt.Sprintf(
			"run %s is waiting on opt_in_removed: the opt-in label was removed from its source issue, which withdraws consent to work on it. Restore the label; the run then resumes through the ordinary schedule. Resuming does not restore consent", runID), true
	}
	if projection, err := runtime.Project(events); err == nil {
		if a := projection.Assurance; a != nil && !a.Stale && a.FailureClass == runtime.FailureWorkspaceIntegrity {
			return runtime.ExitFailed, fmt.Sprintf(
				"run %s stopped on workspace_integrity_violation against its current candidate: the runtime-owned workspace does not match the metadata baseline the runtime recorded. There is no silent resume over a mismatch; inspect the workspace with `autonomy events %s`, then start new work with `autonomy run issue <number>`", runID, runID), true
		}
	}
	return 0, "", false
}

// persistedDisposition is the run's durable disposition. The run document is
// authoritative when one was read; otherwise the journal is folded, which is
// the same ordering journalExitCode uses.
func persistedDisposition(run runtime.EngineeringRun, events []runtime.EngineeringEvent) (runtime.Disposition, string) {
	if run.ID != "" {
		return run.Disposition, run.Reason
	}
	disposition, reason := runtime.Active, ""
	for _, event := range events {
		var payload struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(event.Payload, &payload)
		switch event.Type {
		case runtime.EventRunCompleted:
			disposition, reason = runtime.Completed, payload.Reason
		case runtime.EventRunWaiting:
			disposition, reason = runtime.Waiting, payload.Reason
		case runtime.EventRunFailed:
			disposition, reason = runtime.Failed, payload.Reason
		case runtime.EventRunCancelled:
			disposition, reason = runtime.Cancelled, payload.Reason
		}
	}
	return disposition, reason
}

// ---------------------------------------------------------------------------
// refresh
// ---------------------------------------------------------------------------

// refreshReason is the recorded cause of the generation boundary an explicit
// source refresh draws. It is deliberately not stopReason: a refresh is not a
// person abandoning the work, and the two must stay distinguishable in the
// journal forever.
const refreshReason = "operator_source_refresh"

// refreshView is what `refresh` reports: which run was refreshed, which
// snapshot it had pinned, which run now answers the same source, and which
// snapshot THAT pinned. Both digests are read back from the runtime's own
// status projection, so the operator is shown identity the runtime derived.
type refreshView struct {
	SchemaVersion string                   `json:"schema_version"`
	View          string                   `json:"view"`
	Run           string                   `json:"run"`
	Issue         int                      `json:"issue"`
	SourceBefore  string                   `json:"source_digest_before"`
	SourceAfter   string                   `json:"source_digest_after"`
	Changed       bool                     `json:"source_changed"`
	Successor     string                   `json:"successor_run"`
	Operator      runtime.RecordedOperator `json:"operator"`
	Outcome       runtime.Outcome          `json:"outcome"`
}

// autonomyRefresh is the ONLY way changed source intent is ever re-read. It is
// explicit, it is recorded, and it grants nothing: no permission is added, no
// privilege is expanded, and nothing is published by it.
//
// The shape is `autonomy refresh <run>` rather than `resume --refresh-source`
// on purpose. Keeping it off `resume` is what makes "a plain resume must not
// absorb changed source intent" structurally true instead of flag-dependent:
// there is no flag on resume that could ever do it.
//
// What it does, in order:
//
//  1. reads the run's pinned snapshot identity from the runtime;
//  2. records operator refresh intent durably in the run's OWN journal, as a
//     cancellation whose reason is refreshReason - which is what stales the
//     prior contract, the prior evidence and the prior authority: all of them
//     are bound to that run, and it is now terminal;
//  3. asks the runtime for the run that answers the same source issue, which
//     is the next generation of the same deterministic identity;
//  4. reconciles it normally. Fetching the current source, snapshotting it,
//     and rerunning source -> facts -> policy -> WorkContract are the
//     runtime's own source.observe and contract.compile operations, driven by
//     ordinary reconciliation; this command performs none of them itself.
//
// The old journal is preserved exactly: nothing is rewritten, truncated, or
// re-pointed, and the refreshed run is reachable from it by identity.
//
// ponytail: the boundary is a generation, so a refresh abandons the previous
// candidate branch and any pull request opened from it. An IN-PLACE refresh -
// same run, same branch, recompiled contract - needs three runtime changes this
// change does not own: a source.refreshed event that clears the sticky
// RunProjection.SourceIntentChanged, a contract.compile binding that follows
// the refreshed snapshot instead of the first one, and a contract revision
// derived from the source digest so prior evidence and authority actually
// stale. Add those, then move this command onto them.
func autonomyRefresh(ctx context.Context, engine engineeringRuntime, built *composition, flags autonomyFlags, runID string, stdout io.Writer) (int, error) {
	run, err := requireRun(built, runID)
	if err != nil {
		return exitFor(err, runtime.ExitFailed), err
	}
	operator, err := operatorIdentity(built)
	if err != nil {
		return exitFor(err, runtime.ExitInvalid), err
	}
	before, err := engine.Status(runID)
	if err != nil {
		return exitFor(err, runtime.ExitFailed), err
	}
	issue := before.Source.Issue
	if issue == 0 {
		if issue, err = issueNumberOf(run.Goal, before.Goal); err != nil {
			return runtime.ExitInvalid, err
		}
	}
	if before.Disposition == runtime.Completed {
		return runtime.ExitCompleted, fmt.Errorf("run %s already completed; refreshing it would reopen settled work. Start new work with `autonomy run issue %d`", runID, issue)
	}
	view := refreshView{
		SchemaVersion: runtime.SchemaVersion,
		View:          "autonomy.refresh/1",
		Run:           runID,
		Issue:         issue,
		SourceBefore:  before.Source.Digest,
		Operator:      operator,
	}

	// Durable operator refresh intent. It is idempotent through cancelRun, so
	// a retry after a crash records nothing twice.
	if built != nil {
		if _, err := cancelRun(built, runID, refreshReason); err != nil {
			return exitFor(err, runtime.ExitFailed), err
		}
	}

	successor, err := engine.StartOrResumeIssueRun(ctx, issue)
	if err != nil {
		return exitFor(err, runtime.ExitFailed), err
	}
	view.Successor = successor

	outcome, err := engine.Reconcile(ctx, successor)
	if err != nil {
		return exitFor(err, runtime.ExitFailed), err
	}
	view.Outcome = outcome
	if after, err := engine.Status(successor); err == nil {
		view.SourceAfter = after.Source.Digest
	}
	view.Changed = view.SourceAfter != "" && view.SourceAfter != view.SourceBefore
	if err := writeJSON(stdout, view); err != nil {
		return runtime.ExitFailed, err
	}
	return outcome.ExitCode(), nil
}

// issueNumberOf recovers the source issue from the run's durable goal, which is
// runtime-authored text of the form "github-issue:owner/name#N".
func issueNumberOf(goals ...string) (int, error) {
	for _, goal := range goals {
		_, number, ok := strings.Cut(goal, "#")
		if !ok {
			continue
		}
		if issue, err := strconv.Atoi(strings.TrimSpace(number)); err == nil && issue > 0 {
			return issue, nil
		}
	}
	return 0, errors.New("the run does not name a source issue, so there is no source to refresh")
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

// statusView is the primary operator view. It is a PROJECTION over persisted
// state - the runtime's own StatusReport, the folded journal, and the durable
// watch observation for the run's repository - and it is deterministic: the
// same durable state always renders the same view. Nothing here is a summary
// produced by a model, and nothing here is re-decided.
//
// The JSON is versioned by View; the text rendering is a projection over this
// same structure, so the two can never describe different state.
type statusView struct {
	runtime.StatusReport
	View string `json:"view"`

	// Lease is who currently holds the run-driving lease, if anyone.
	Lease *leaseView `json:"lease,omitempty"`
	// Authority is every action the run has a decision for, BY ACTION, in a
	// deterministic order.
	Authority []authorityActionView `json:"authority,omitempty"`
	// AuthorityRequest is the exact request an operator would answer.
	AuthorityRequest *runtime.AuthorityRequest `json:"authority_request,omitempty"`
	// AuthorityRefusal is the typed refusal raised when the request could not
	// even be projected - a changed controller, moved source, an external head.
	AuthorityRefusal string `json:"authority_refusal,omitempty"`
	// StaleAuthorityRequests are requests this run recorded an answer for that
	// are not the current request. They are the record of approvals that no
	// longer bind, which is why they are shown rather than dropped.
	StaleAuthorityRequests []runtime.Ref `json:"stale_authority_requests,omitempty"`
	// MetadataIntegrity is the runtime-owned .git metadata baseline state.
	MetadataIntegrity metadataIntegrityView `json:"metadata_integrity"`
	// GitHub is the durable forge observation for this repository: the auth or
	// rate-limit wait an operator has to act on.
	GitHub *githubWaitView `json:"github,omitempty"`
	// NextAction is the one thing the operator should do next. It is a total
	// function of everything above.
	NextAction string `json:"next_action"`
}

type leaseView struct {
	Owner       string        `json:"owner"`
	Operation   string        `json:"operation"`
	HeartbeatAt *time.Time    `json:"heartbeat_at,omitempty"`
	ExpiresAt   *time.Time    `json:"expires_at,omitempty"`
	Elapsed     time.Duration `json:"elapsed"`
}

type authorityActionView struct {
	Action   string                 `json:"action"`
	Decision runtime.Ref            `json:"decision"`
	Status   domain.AuthorityStatus `json:"status"`
	Sequence int64                  `json:"sequence"`
}

type metadataIntegrityView struct {
	// Baseline is the trusted .git metadata digest as of the last runtime-owned
	// Git operation that succeeded. Empty means no baseline is established for
	// the current head, which is not itself a violation.
	Baseline string `json:"baseline,omitempty"`
	// ObservedExternalHead is a candidate head observed externally that is not
	// the head the runtime recorded.
	ObservedExternalHead string `json:"observed_external_head,omitempty"`
	// Violation is true when the run's current head carries a recorded
	// workspace integrity failure, or an external head was observed.
	Violation bool `json:"violation"`
}

type githubWaitView struct {
	ErrorClass          runtime.WatchErrorClass `json:"error_class,omitempty"`
	Detail              string                  `json:"detail,omitempty"`
	NotBefore           time.Time               `json:"not_before,omitzero"`
	ConsecutiveFailures int                     `json:"consecutive_failures,omitempty"`
	RateLimit           runtime.WatchRateLimit  `json:"rate_limit,omitzero"`
}

// autonomyStatus renders the run. The exit status is the run's own disposition,
// through the same mapping every other command uses, so `status` and `run` can
// never disagree about what a disposition exits as.
func autonomyStatus(engine engineeringRuntime, built *composition, flags autonomyFlags, runID string, stdout io.Writer) (int, error) {
	if _, err := requireRun(built, runID); err != nil {
		return exitFor(err, runtime.ExitFailed), err
	}
	report, err := engine.Status(runID)
	if err != nil {
		return exitFor(err, runtime.ExitFailed), err
	}
	events, err := engine.Journal(runID)
	if err != nil {
		return exitFor(err, runtime.ExitFailed), err
	}
	view := statusView{StatusReport: report, View: "autonomy.status/1"}

	request, requestErr := engine.PendingAuthorityRequest(runID)
	var refused *runtime.AuthorityRefusedError
	switch {
	case errors.As(requestErr, &refused):
		view.AuthorityRefusal = refused.Code + ": " + refused.Detail
	case requestErr != nil:
		return exitFor(requestErr, runtime.ExitFailed), requestErr
	default:
		view.AuthorityRequest = request
	}

	projection, err := runtime.Project(events)
	if err != nil {
		return runtime.ExitFailed, err
	}
	view.Authority = authorityByAction(projection)
	view.StaleAuthorityRequests = staleAuthorityRequests(events, request)
	view.Lease = leaseOf(events, report)
	view.MetadataIntegrity = metadataIntegrityView{
		Baseline:             projection.CandidateMetadata,
		ObservedExternalHead: projection.ObservedExternalHead,
		Violation: projection.ObservedExternalHead != "" ||
			(projection.Assurance != nil && !projection.Assurance.Stale && projection.Assurance.FailureClass == runtime.FailureWorkspaceIntegrity),
	}
	view.GitHub = githubWaitOf(built, report.Repository)
	view.NextAction = nextOperatorAction(view)

	if flags.Text {
		if err := renderStatusText(stdout, view); err != nil {
			return runtime.ExitFailed, err
		}
	} else if err := writeJSON(stdout, view); err != nil {
		return runtime.ExitFailed, err
	}
	return runtime.Outcome{RunID: runID, Disposition: report.Disposition, Reason: report.Reason}.ExitCode(), nil
}

// authorityByAction is every recorded decision, one per action, ordered so two
// reads of the same journal render identically.
func authorityByAction(projection runtime.RunProjection) []authorityActionView {
	out := make([]authorityActionView, 0, len(projection.AuthorityDecisions))
	for _, decision := range projection.AuthorityDecisions {
		out = append(out, authorityActionView{
			Action:   decision.Action.Type + ":" + decision.Action.Target,
			Decision: decision.Decision,
			Status:   decision.Status,
			Sequence: decision.Sequence,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Action < out[j].Action })
	return out
}

// staleAuthorityRequests are the requests this run recorded a human answer for
// that are not the request it currently produces. An approval is contextual
// evidence, never a standing permission, so a superseded one is REPORTED rather
// than quietly dropped: it is still in the journal and it still binds nothing.
func staleAuthorityRequests(events []runtime.EngineeringEvent, current *runtime.AuthorityRequest) []runtime.Ref {
	seen, out := map[runtime.Ref]bool{}, []runtime.Ref(nil)
	for _, event := range events {
		if event.Type != runtime.EventHumanAuthorityRecorded {
			continue
		}
		var payload runtime.HumanAuthorityRecordedPayload
		if json.Unmarshal(event.Payload, &payload) != nil {
			continue
		}
		if current != nil && payload.Request.ID == current.ID {
			continue
		}
		if seen[payload.Request] {
			continue
		}
		seen[payload.Request] = true
		out = append(out, payload.Request)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// leaseOf reports who holds the run-driving lease. The lease owner lives in the
// operation document the journal records, so it is read from there rather than
// from a second source.
func leaseOf(events []runtime.EngineeringEvent, report runtime.StatusReport) *leaseView {
	if report.Operation == nil {
		return nil
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != runtime.EventOperationBefore && event.Type != runtime.EventOperationAfter {
			continue
		}
		var operation runtime.RunOperation
		if json.Unmarshal(event.Payload, &operation) != nil || operation.ID != report.Operation.ID {
			continue
		}
		if operation.Lease == nil {
			return nil
		}
		heartbeat, expires := operation.Lease.HeartbeatAt, operation.Lease.ExpiresAt
		return &leaseView{
			Owner:       operation.Lease.Owner,
			Operation:   operation.ID,
			HeartbeatAt: &heartbeat,
			ExpiresAt:   &expires,
			Elapsed:     report.Operation.Elapsed,
		}
	}
	return nil
}

// githubWaitOf reports the durable forge observation for this repository. It is
// read, never probed: status makes no network call.
func githubWaitOf(built *composition, repository string) *githubWaitView {
	if built == nil || strings.TrimSpace(repository) == "" {
		return nil
	}
	state, _, found, err := built.store.WatchStateFor(repository)
	if err != nil || !found {
		return nil
	}
	if state.LastErrorClass == runtime.WatchErrorNone && state.RateLimit.Remaining > 0 {
		return nil
	}
	return &githubWaitView{
		ErrorClass:          state.LastErrorClass,
		Detail:              state.LastErrorDetail,
		NotBefore:           state.NotBefore,
		ConsecutiveFailures: state.ConsecutiveFailures,
		RateLimit:           state.RateLimit,
	}
}

// providerAccountWaitReason is the durable run reason runtime settles into when
// the execution provider refuses at its account boundary. It is matched by
// value here because the CLI renders durable state; it does not re-derive it.
const providerAccountWaitReason = "execution_provider_account_unavailable"

// nextOperatorAction is the one interpretive field in the view, and it is a
// total function of the rest of it: the same durable state always yields the
// same sentence. It grants nothing and decides nothing.
func nextOperatorAction(view statusView) string {
	run := view.RunID
	switch {
	case view.Disposition == runtime.Completed:
		return "none: the run reached its goal"
	case view.Disposition == runtime.Cancelled:
		return "none: the run was cancelled (" + view.Reason + "). Start new work with `autonomy run issue <number>`"
	// The provider-account wait answers before the authority branch on purpose.
	// It is an EXTERNAL account prerequisite, not a human-authority condition,
	// and telling an operator that the authority boundary is refusing would
	// send them to resolve a condition that does not exist.
	case view.Reason == providerAccountWaitReason:
		return "restore execution-provider account availability, then `autonomy resume " + run + "`"
	case view.AuthorityRefusal != "":
		return "the human-authority boundary is refusing to project a request (" + view.AuthorityRefusal + "); resolve the named condition first"
	case view.AuthorityRequest != nil:
		return "record a human decision: `autonomy authorize " + run + " " + view.AuthorityRequest.ID + " --approve` or `--reject`"
	case view.MetadataIntegrity.Violation:
		return "the candidate's integrity is unproven; inspect `autonomy events " + run + "` before doing anything else"
	}
	switch view.Reason {
	case "source_intent_changed":
		return "the pinned source moved; re-read it explicitly with `autonomy refresh " + run + "`. A plain resume will not absorb it"
	case runtime.WatchWaitingOptInRemoved:
		return "restore the opt-in label on the source issue; consent is what the run is waiting for"
	case runtime.WatchWaitingGitHubAuth:
		return "restore the forge credential (`gh auth login`), then `autonomy resume " + run + "`"
	case "controller_changed":
		return "the run was created by a different controller or configuration; restore that configuration or start new work with `autonomy run issue <number>`"
	case "requested_privilege_expansion":
		return "policy does not grant the observed scope's permission; run `autonomy doctor` and fix the policy, then start new work"
	case "candidate_external_changed":
		return "the candidate head was changed outside the runtime; inspect it. No approval can be given past this"
	}
	if view.GitHub != nil && view.GitHub.ErrorClass == runtime.WatchErrorRateLimited {
		return "the forge rate-limit budget is exhausted until " + view.GitHub.RateLimit.ResetAt.UTC().Format(time.RFC3339) + "; no action is needed before then"
	}
	if view.Disposition == runtime.Failed {
		if d := view.ExecutionDiagnostic; d != nil {
			return "the run failed (" + view.Reason + ") at execution stage " + d.Stage +
				"; the sanitized diagnostic is shown above and `autonomy events " + run + "` holds the full journal"
		}
		return "inspect `autonomy events " + run + "`; the run failed (" + view.Reason + ")"
	}
	return "`autonomy resume " + run + "` to ask the runtime to reconcile again"
}

// renderStatusText is the human projection over the SAME structure the JSON
// carries. It adds no field and hides no refusal.
func renderStatusText(stdout io.Writer, view statusView) error {
	line := func(label string, value any) {
		fmt.Fprintf(stdout, "%-22s %v\n", label+":", value)
	}
	build := view.Controller.Build
	line("run", view.RunID)
	line("repository", view.Repository)
	if view.Source.Issue != 0 {
		line("source", fmt.Sprintf("issue #%d %s (%s) digest=%s intent_changed=%t",
			view.Source.Issue, view.Source.URL, view.Source.State, short(view.Source.Digest), view.Source.IntentChanged))
	}
	line("controller", fmt.Sprintf("%s sha=%s changed=%t", view.Controller.ID, short(view.Controller.SHA256), view.Controller.Changed))
	// The provenance of the build that CREATED the run, and the configuration
	// digest, are shown as separate lines because they are separate facts: a
	// digest alone cannot say whether the binary or the configuration moved.
	line("controller build", fmt.Sprintf("%s version=%s source=%s tree=%s binary=%s",
		build.Kind, build.Version, short(build.SourceRevision), short(build.SourceTree), short(build.BinarySHA256)))
	line("controller config", fmt.Sprintf("global=%s repository=%s",
		short(view.Controller.ConfigDigest.Global), short(view.Controller.ConfigDigest.Repository)))
	line("disposition", strings.TrimSpace(string(view.Disposition)+" "+view.Reason))
	line("phase", view.Phase)
	line("base", view.Base.ID+"@"+short(view.Base.Revision))
	line("candidate", fmt.Sprintf("%s rev=%s tree=%s", view.Candidate.Branch, short(view.Candidate.Revision), short(view.Candidate.Tree)))
	line("contract", view.Contract.ID+"@"+view.Contract.Revision)
	if view.Operation != nil {
		line("operation", fmt.Sprintf("%s %s attempt %d/%d elapsed %s",
			view.Operation.Kind, view.Operation.State, view.Operation.Attempt, view.Operation.MaxAttempts, view.Operation.Elapsed))
	}
	if view.Lease != nil {
		heartbeat := "never"
		if view.Lease.HeartbeatAt != nil {
			heartbeat = view.Lease.HeartbeatAt.UTC().Format(time.RFC3339)
		}
		line("lease", fmt.Sprintf("%s heartbeat %s elapsed %s", view.Lease.Owner, heartbeat, view.Lease.Elapsed))
	}
	for _, evidence := range view.Evidence {
		line("evidence", evidence.ID+"@"+evidence.Revision)
	}
	for _, authority := range view.Authority {
		line("authority", fmt.Sprintf("%s %s (%s)", authority.Action, authority.Status, authority.Decision.ID))
	}
	if view.AuthorityRequest != nil {
		line("authority request", fmt.Sprintf("%s requires %s", view.AuthorityRequest.ID, strings.Join(view.AuthorityRequest.Requires, ", ")))
	}
	if view.AuthorityRefusal != "" {
		line("authority refused", view.AuthorityRefusal)
	}
	for _, stale := range view.StaleAuthorityRequests {
		line("stale request", stale.ID)
	}
	if view.PullRequest != nil {
		line("pull request", fmt.Sprintf("#%d head=%s state=%s merged=%t stale=%t",
			view.PullRequest.Number, short(view.PullRequest.HeadRevision), view.PullRequest.State, view.PullRequest.Merged, view.PullRequest.Stale))
	}
	line("metadata integrity", fmt.Sprintf("baseline=%s external_head=%s violation=%t",
		short(view.MetadataIntegrity.Baseline), short(view.MetadataIntegrity.ObservedExternalHead), view.MetadataIntegrity.Violation))
	if view.GitHub != nil {
		line("github", fmt.Sprintf("%s %s remaining=%d not_before=%s",
			view.GitHub.ErrorClass, view.GitHub.Detail, view.GitHub.RateLimit.Remaining, view.GitHub.NotBefore.UTC().Format(time.RFC3339)))
	}
	if attempts := sortedAttempts(view.Attempts); attempts != "" {
		line("attempts", attempts)
	}
	// The execution diagnostic is rendered as bounded FIELDS, never as provider
	// output: everything printed here is already sanitized and length-bounded
	// where it was persisted. The artifact is named, never opened - it is
	// local-only material and printing it would publish exactly what the
	// sanitization exists to keep out of an operator-visible surface.
	if d := view.ExecutionDiagnostic; d != nil {
		failure := strings.TrimSpace(fmt.Sprintf("stage=%s class=%s route=%s %s",
			d.Stage, d.FailureClass, d.Route, d.Code))
		if d.FailureClass == runtime.FailureProviderAccountUnavailable {
			failure = "provider account unavailable (" + failure + ")"
		}
		line("execution failure", failure)
		line("execution provider", strings.TrimSpace(fmt.Sprintf("%s model=%s", d.ProviderKind, d.Model)))
		if d.HTTPStatus != 0 || d.ProviderErrorCode != "" || d.ProviderErrorParam != "" {
			line("execution response", strings.TrimSpace(fmt.Sprintf("http=%d provider_error=%s param=%s",
				d.HTTPStatus, d.ProviderErrorCode, d.ProviderErrorParam)))
		}
		if d.Message != "" {
			line("execution detail", d.Message)
		}
		if d.ArtifactRef != "" {
			line("execution artifact", d.ArtifactRef+" (local-only, sanitized; not published)")
		}
	}
	line("next action", view.NextAction)
	return nil
}

// sortedAttempts renders the per-kind attempt counters in a fixed order, so two
// renderings of the same run are byte-identical.
func sortedAttempts(attempts map[string]int) string {
	kinds := make([]string, 0, len(attempts))
	for kind := range attempts {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	rendered := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		rendered = append(rendered, fmt.Sprintf("%s=%d", kind, attempts[kind]))
	}
	return strings.Join(rendered, " ")
}

func short(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}

// ---------------------------------------------------------------------------
// events
// ---------------------------------------------------------------------------

// maxEventPayloadBytes bounds ONE rendered payload. Canonical payloads are
// already bounded by the journal, so this is a display bound: it keeps a
// follower's output readable and keeps the renderer honest about the fact that
// it prints a SUMMARY, never a body.
const maxEventPayloadBytes = 1024

// followInterval is how often `--follow` re-reads the journal. It is a polite
// poll of a local SQLite file, not a subscription: no lock is taken, no
// notification channel is opened, and a controller appending concurrently is
// simply observed on the next pass.
const followInterval = time.Second

// eventView renders one journalled event. Every member is journal identity,
// journal position, or a bounded summary. Artifact CONTENT is never printed:
// what is rendered is the artifact REFERENCE the journal itself holds - a path,
// a digest, and its local-only and sanitized flags - so listing events can
// never emit raw local-only material.
type eventView struct {
	SchemaVersion     string             `json:"schema_version"`
	View              string             `json:"view"`
	Sequence          int64              `json:"sequence"`
	ID                string             `json:"id"`
	RunID             string             `json:"run_id"`
	Type              string             `json:"type"`
	OccurredAt        time.Time          `json:"occurred_at"`
	Actor             string             `json:"actor"`
	OperationID       string             `json:"operation_id,omitempty"`
	StateBefore       string             `json:"state_before,omitempty"`
	StateAfter        string             `json:"state_after,omitempty"`
	Subject           *eventSubject      `json:"subject,omitempty"`
	Payload           json.RawMessage    `json:"payload,omitempty"`
	PayloadSummary    *payloadSummary    `json:"payload_summary,omitempty"`
	Artifacts         []runtime.Artifact `json:"artifacts,omitempty"`
	PreviousEventID   string             `json:"previous_event_id,omitempty"`
	PreviousEventHash string             `json:"previous_event_hash,omitempty"`
	EventHash         string             `json:"event_hash,omitempty"`
}

// eventSubject is the exact state an event is bound to, when its payload names
// one. It is what makes a journal line answerable without opening the payload.
type eventSubject struct {
	Commit    string             `json:"commit,omitempty"`
	Tree      string             `json:"tree,omitempty"`
	Contract  *runtime.Ref       `json:"contract,omitempty"`
	Candidate *runtime.Candidate `json:"candidate,omitempty"`
	Action    string             `json:"action,omitempty"`
	Request   *runtime.Ref       `json:"request,omitempty"`
}

// payloadSummary replaces a payload too large to render. It states the size and
// the digest of what was withheld rather than truncating JSON into something
// that no longer parses.
type payloadSummary struct {
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// autonomyEvents renders the persisted journal in persisted order. It opens the
// durable store DIRECTLY: it takes no ownership of the state directory and no
// run-driving lease, which is what makes it safe - and correct - to read a run
// another controller is currently driving.
func autonomyEvents(parent context.Context, flags autonomyFlags, runID string, stdout io.Writer) (int, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return runtime.ExitInvalid, err
	}
	config, err := runtime.LoadConfig(flags.Config, cwd)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	store, err := runtime.OpenSQLiteOperationStore(config.StateDir)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer store.Close()

	if _, found, err := store.Run(runID); err != nil {
		return runtime.ExitFailed, err
	} else if !found {
		err := &runNotFoundError{RunID: runID}
		return exitRunNotFound, err
	}
	if !flags.Follow {
		events, err := store.Events(runID)
		if err != nil {
			return runtime.ExitFailed, err
		}
		if err := writeJSON(stdout, eventViews(events)); err != nil {
			return runtime.ExitFailed, err
		}
		return runtime.ExitCompleted, nil
	}
	return followEvents(parent, store, runID, stdout)
}

// followEvents tails the journal. It mutates nothing: the only call it makes is
// the same read the one-shot path makes, so a concurrent append by an
// independent handle is simply the next thing it observes. It exits cleanly on
// SIGINT or SIGTERM, which is a shutdown of THIS reader and never a
// cancellation of the run.
func followEvents(parent context.Context, store *runtime.SQLiteOperationStore, runID string, stdout io.Writer) (int, error) {
	ctx, stopSignals := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	var last int64
	ticker := time.NewTicker(followInterval)
	defer ticker.Stop()
	for {
		events, err := store.Events(runID)
		if err != nil {
			return runtime.ExitFailed, err
		}
		for _, event := range events {
			if event.Sequence <= last {
				continue
			}
			last = event.Sequence
			if err := writeJSON(stdout, renderEvent(event)); err != nil {
				return runtime.ExitFailed, err
			}
		}
		select {
		case <-ctx.Done():
			return runtime.ExitCompleted, nil
		case <-ticker.C:
		}
	}
}

func eventViews(events []runtime.EngineeringEvent) []eventView {
	out := make([]eventView, 0, len(events))
	for _, event := range events {
		out = append(out, renderEvent(event))
	}
	return out
}

// renderEvent renders one journalled row. Nothing is re-ordered, nothing is
// filtered, and no artifact body is opened.
func renderEvent(event runtime.EngineeringEvent) eventView {
	view := eventView{
		SchemaVersion:     event.SchemaVersion,
		View:              "autonomy.events/1",
		Sequence:          event.Sequence,
		ID:                event.ID,
		RunID:             event.RunID,
		Type:              event.Type,
		OccurredAt:        event.OccurredAt,
		Actor:             eventActor(event),
		OperationID:       event.OperationID,
		StateBefore:       event.StateBefore,
		StateAfter:        event.StateAfter,
		Subject:           eventSubjectOf(event.Payload),
		Artifacts:         event.Artifacts,
		PreviousEventID:   event.PreviousEventID,
		PreviousEventHash: event.PreviousEventHash,
		EventHash:         event.EventHash,
	}
	if len(event.Payload) > maxEventPayloadBytes {
		sum := sha256.Sum256(event.Payload)
		view.PayloadSummary = &payloadSummary{Bytes: len(event.Payload), SHA256: hex.EncodeToString(sum[:])}
		return view
	}
	view.Payload = event.Payload
	return view
}

// eventActor is where the event came from. An operator-appended event carries
// no operation id, because recording operator intent is not an engineering side
// effect and never takes an operation lease; that is what distinguishes it.
func eventActor(event runtime.EngineeringEvent) string {
	switch event.Type {
	case runtime.EventHumanAuthorityRecorded:
		return "operator"
	case runtime.EventRunCancelled:
		if event.OperationID == "" {
			return "operator"
		}
	case runtime.EventGitHubPRObserved, runtime.EventGitHubCIObserved, runtime.EventGitHubReviewObserved,
		runtime.EventSourceIntentChanged, runtime.EventSourceOptInRemoved, runtime.EventSourceOptInRestored:
		return "forge"
	}
	return "runtime"
}

// eventSubjectOf reads the exact state binding a payload names, if it names
// one. It decodes the shared member names rather than every payload type, so a
// payload the renderer has never heard of still reports its binding.
func eventSubjectOf(payload json.RawMessage) *eventSubject {
	if len(payload) == 0 {
		return nil
	}
	var bound struct {
		Commit    string             `json:"commit"`
		Tree      string             `json:"tree"`
		Contract  *runtime.Ref       `json:"contract"`
		Candidate *runtime.Candidate `json:"candidate"`
		Action    *domain.Action     `json:"action"`
		Request   *runtime.Ref       `json:"request"`
	}
	if json.Unmarshal(payload, &bound) != nil {
		return nil
	}
	subject := eventSubject{
		Commit: bound.Commit, Tree: bound.Tree,
		Contract: bound.Contract, Candidate: bound.Candidate, Request: bound.Request,
	}
	if bound.Action != nil {
		subject.Action = bound.Action.Type + ":" + bound.Action.Target
	}
	if subject == (eventSubject{}) {
		return nil
	}
	return &subject
}

// ---------------------------------------------------------------------------
// doctor
// ---------------------------------------------------------------------------

// autonomyDoctor answers, per capability, whether the thing a real run depends
// on is actually there. Every question is answered by runtime.Doctor; this
// function only wires the dependency set and maps the worst verdict onto an
// exit status.
//
// It never returns early on a configuration failure. A preflight whose whole
// purpose is to explain a broken installation must still run when the operator
// configuration does not load, so the input is assembled leniently and Doctor's
// own configuration checks report what failed.
func autonomyDoctor(args []string, overrides autonomyOverrides, stdout io.Writer) (int, error) {
	flags, err := parseAutonomyFlags(args)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	report := runtime.Doctor(context.Background(), doctorInput(flags, overrides))
	if flags.Text {
		for _, check := range report.Checks {
			fmt.Fprintf(stdout, "%-4s %-34s %s\n", check.Status, check.ID, check.Reason)
		}
		fmt.Fprintln(stdout, string(report.Status))
	} else if err := writeJSON(stdout, report); err != nil {
		return runtime.ExitFailed, err
	}
	if report.Status == runtime.DoctorFail {
		return runtime.ExitFailed, nil
	}
	return runtime.ExitCompleted, nil
}

// doctorInput is the SAME wiring newComposition performs, assembled without
// opening the durable store and without taking the state directory's ownership
// lock: a diagnosis must not take the resource it is diagnosing, and it must
// not refuse to run because a second process already holds it.
func doctorInput(flags autonomyFlags, overrides autonomyOverrides) runtime.DoctorInput {
	cwd, _ := os.Getwd()
	in := runtime.DoctorInput{
		OperatorConfigPath: flags.Config,
		RepositoryRoot:     cwd,
		Codex:              runtime.NativeCodexProvider{},
		GitHub:             overrides.GitHub,
		Provider:           overrides.Provider,
	}
	// The running binary's own provenance. A build that cannot resolve it
	// reports the unattested zero value rather than nothing, because "I do not
	// know what I am" is itself the answer to the check.
	if overrides.ControllerBuild != nil {
		in.ControllerBuild = *overrides.ControllerBuild
	} else if build, err := controllerBuild(); err == nil {
		in.ControllerBuild = build
	}
	if target, err := repositoryTarget(cwd, flags.Repo); err == nil {
		in.Repository = target
	}
	config, err := runtime.LoadConfig(flags.Config, cwd)
	if err != nil {
		return in
	}
	sandbox := runtime.DockerSandbox{Image: config.Assurance.Image, Endpoint: runtime.DockerEndpoint{Host: config.Assurance.DockerHost}}
	artifacts := runtime.ArtifactStore{Root: filepath.Join(config.StateDir, "artifacts")}
	in.StateDir = config.StateDir
	in.Sandbox = sandbox
	in.DependencyCacheDir = config.Assurance.DependencyCacheDir
	in.SemanticAssurance = overrides.SemanticAssurance
	if in.SemanticAssurance == nil {
		in.SemanticAssurance = semanticAssuranceProvider(config, artifacts)
	}
	in.ProviderCredentialPath = config.Provider.CredentialPath
	in.GitHubCredentialMode = config.GitHub.CredentialMode
	in.DiscoveryLabel = config.Watch.Label
	in.Credentials = githubCredentials(config.GitHub.CredentialMode)
	if in.Provider == nil {
		in.Provider = executionProvider(config, artifacts, sandbox)
	}
	if in.GitHub == nil && in.Credentials != nil {
		in.GitHub = runtime.GitHubRESTAdapter{
			HTTP:        &http.Client{Timeout: 30 * time.Second},
			Endpoint:    config.GitHub.Endpoint,
			Credentials: in.Credentials,
		}
	}
	if model, err := analysis.LoadProjectModel(config.ProjectModelPath); err == nil {
		in.ProjectModel = model
	}
	if policy, err := runtime.LoadEngineeringPolicy(config.PolicyPath); err == nil {
		in.Policy = policy
	}
	return in
}

// ---------------------------------------------------------------------------
// gc
// ---------------------------------------------------------------------------

// autonomyGC reclaims heavyweight local material under the runtime state
// directory. There is exactly ONE planner: --dry-run prints Plan(), and a real
// run calls Collect(), which plans with the same function and revalidates every
// target immediately before deleting it. The two can therefore never disagree
// about what is eligible.
//
// It takes the state directory's ownership lock through the composition root,
// because unlike every other operator read this command DELETES.
func autonomyGC(args []string, overrides autonomyOverrides, stdout io.Writer) (int, error) {
	flags, err := parseAutonomyFlags(args)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	built, err := newComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer built.release()

	collector := runtime.Collector{
		Store:     built.store,
		Clock:     runtime.RealClock{},
		StateDir:  built.config.StateDir,
		Retention: built.config.GCRetention(),
	}
	var rendered any
	if flags.DryRun {
		rendered, err = collector.Plan()
	} else {
		rendered, err = collector.Collect()
	}
	if err != nil {
		return runtime.ExitFailed, err
	}
	if err := writeJSON(stdout, rendered); err != nil {
		return runtime.ExitFailed, err
	}
	return runtime.ExitCompleted, nil
}
