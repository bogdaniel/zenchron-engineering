package agentkernel

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/execution"
)

// fullObjectName is a full Git object name, the only revision or tree form
// the kernel's workspace binding accepts.
var fullObjectName = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// RefusalError is the adapter refusing a request before the kernel saw it:
// a host field the kernel cannot honour, or a host bound it cannot represent
// without widening. Nothing ran and nothing was admitted.
type RefusalError struct {
	Field  string
	Reason string
}

func (e *RefusalError) Error() string {
	return "agent execution kernel refused the request: " + e.Field + ": " + e.Reason
}

func refuse(field, format string, args ...any) error {
	return &RefusalError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// translate maps one host request onto one kernel request. Every host field
// either maps, is refused, or is host-only (consumed by the host before or
// after Execute and meaningless inside one bounded execution); the table in
// docs/acceptance/agent-execution-kernel-gate-b.md lists each. No bound is
// ever defaulted wider than the host stated: the adapter's own Limits only
// narrow, and a host bound that cannot be represented refuses.
func (a *Adapter) translate(req execution.Request, now time.Time, ctxDeadline time.Time) (api.ExecutionRequest, error) {
	ref := req.AttemptRef()
	if err := ref.Validate(); err != nil {
		return api.ExecutionRequest{}, refuse("attempt", "%v", err)
	}
	if err := refuseResultSlots(req); err != nil {
		return api.ExecutionRequest{}, err
	}
	if req.ModelPreference != "" && req.ModelPreference != a.binding.Model {
		return api.ExecutionRequest{}, refuse("model_preference",
			"%q is not the bound model %q; the kernel adapter never switches models", req.ModelPreference, a.binding.Model)
	}
	mode, err := kernelMode(req.Mode)
	if err != nil {
		return api.ExecutionRequest{}, err
	}
	workspace, err := workspaceRef(req, executionID(ref))
	if err != nil {
		return api.ExecutionRequest{}, err
	}
	budget, err := a.budget(req, now, ctxDeadline)
	if err != nil {
		return api.ExecutionRequest{}, err
	}
	host := hostContext(req)
	digest, err := instructionDigest(host)
	if err != nil {
		return api.ExecutionRequest{}, err
	}
	binding := a.binding
	binding.Eligible, binding.Pinned = true, true
	// Gate B proves no isolation yet: kernel file tools are development-grade
	// and the sandboxed runner is not composed here (integration plan §5.5).
	binding.Isolation = api.IsolationUnproven
	return api.ExecutionRequest{
		Version:     api.ExecutionVersion,
		ExecutionID: executionID(ref),
		AttemptID:   attemptID(ref),
		Objective:   req.Objective,
		Mode:        mode,
		Workspace:   workspace,
		Constraints: api.Constraints{InstructionDigest: digest},
		Context:     append(host, untrustedContext(req)...),
		Grants:      a.grants(mode, req.RequiredTools),
		Budget:      budget,
		Providers:   []api.ProviderBinding{binding},
	}, nil
}

// refuseResultSlots refuses a request that names a runtime-owned result slot.
// The kernel's tools are confined to the candidate, so it could never write
// one, and a reviewer, feedback-resolution, handoff or message protocol that
// silently produced nothing would read as a worker that declined to answer.
func refuseResultSlots(req execution.Request) error {
	for _, slot := range []struct{ field, value string }{
		{"reviewer_result_path", req.ReviewerResultPath},
		{"feedback_resolution_path", req.FeedbackResolutionPath},
		{"handoff_path", req.HandoffPath},
		{"message_path", req.MessagePath},
		{"communication", req.Communication},
	} {
		if slot.value != "" {
			return refuse(slot.field, "the kernel cannot serve a runtime-owned result slot or inbox")
		}
	}
	return nil
}

func kernelMode(mode domain.InvocationMode) (api.Mode, error) {
	switch mode {
	case "", domain.InvocationModeMutating:
		return api.ModeReadWrite, nil
	case domain.InvocationModeNonMutatingPlanning:
		return api.ModeReadOnly, nil
	}
	return "", refuse("mode", "unsupported invocation mode %q", mode)
}

// workspaceRef binds the execution to the candidate tree the host asserted
// the workspace holds before invoking (assertExecutionSubject). The kernel
// has no manifest helper a host can call (integration plan §5.12), so the
// manifest digest is the digest of that exact Git tree id.
func workspaceRef(req execution.Request, id string) (api.WorkspaceRef, error) {
	dir := req.CandidateDir
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return api.WorkspaceRef{}, refuse("candidate_dir", "%q must be a clean absolute path", dir)
	}
	if !fullObjectName.MatchString(req.Candidate.Tree) {
		return api.WorkspaceRef{}, refuse("candidate.tree", "a full Git tree id is required to bind the workspace")
	}
	if req.Candidate.Revision != "" && !fullObjectName.MatchString(req.Candidate.Revision) {
		return api.WorkspaceRef{}, refuse("candidate.revision", "must be a full Git object name")
	}
	return api.WorkspaceRef{
		ID:             id,
		ManifestDigest: api.Digest([]byte("git-tree:" + req.Candidate.Tree)),
		GitRevision:    req.Candidate.Revision,
	}, nil
}

// budget maps every host bound into the kernel's finite envelope. The
// adapter's Limits fill only what the host leaves unstated and only ever
// narrow what it states.
func (a *Adapter) budget(req execution.Request, now, ctxDeadline time.Time) (api.Budget, error) {
	b := req.Budgets
	if b.WallLimit < 0 || b.InactivityLimit < 0 {
		return api.Budget{}, refuse("budgets", "a negative wall or inactivity limit is not a bound")
	}
	deadline := now.Add(a.limits.MaxWall)
	for _, bound := range []time.Time{derefTime(req.Deadline), ctxDeadline, wallInstant(now, b.WallLimit)} {
		if !bound.IsZero() && bound.Before(deadline) {
			deadline = bound
		}
	}
	in, out, err := a.tokenBounds(b.MaxTokens)
	if err != nil {
		return api.Budget{}, err
	}
	money, err := a.moneyCeiling(b.MaxCostMicros)
	if err != nil {
		return api.Budget{}, err
	}
	return api.Budget{
		Deadline:         deadline,
		MaxIterations:    a.limits.MaxIterations,
		MaxToolCalls:     a.limits.MaxToolCalls,
		MaxInputTokens:   in,
		MaxOutputTokens:  out,
		MaxArtifactBytes: a.limits.MaxArtifactBytes,
		// Zero: the host owns retries and durable waits. A kernel retry is
		// immediate, so any allowance here would poll a rate-limited or
		// unavailable provider instead of routing to the host's wait.
		MaxProviderRetries: 0,
		Money:              money,
	}, nil
}

// tokenBounds splits the host's single token total into the kernel's input
// and output bounds so that input + output never exceeds it. The kernel has
// no total dimension, so a total too small to give both a positive share
// cannot be represented and refuses.
func (a *Adapter) tokenBounds(total *int64) (int64, int64, error) {
	in, out := a.limits.MaxInputTokens, a.limits.MaxOutputTokens
	if total == nil {
		return in, out, nil
	}
	if *total < 2 {
		return 0, 0, refuse("budgets.max_tokens", "%d cannot be split into positive input and output bounds", *total)
	}
	out = min(out, *total/2)
	in = min(in, *total-out)
	return in, out, nil
}

// moneyCeiling maps the host's cost ceiling. The kernel enforces money only
// against a complete trusted rate card in the ceiling's currency; without
// one the ceiling would be ignored, so it refuses instead.
func (a *Adapter) moneyCeiling(maxMicros *int64) (*api.MoneyCeiling, error) {
	if maxMicros == nil {
		return nil, nil
	}
	if *maxMicros <= 0 {
		return nil, refuse("budgets.max_cost_micros", "%d is not a positive ceiling", *maxMicros)
	}
	p := a.binding.Pricing
	if a.costCurrency == "" || p == nil || p.Currency != a.costCurrency ||
		p.CachedInputMicrosPerMillion == nil || p.CacheWriteInputMicrosPerMillion == nil {
		return nil, refuse("budgets.max_cost_micros",
			"no complete trusted rate card in the host's cost currency %q, so the ceiling cannot be enforced", a.costCurrency)
	}
	return &api.MoneyCeiling{Currency: a.costCurrency, MaxMicros: *maxMicros}, nil
}

// grants are the only capabilities the execution receives: file tools rooted
// at the candidate, writes only in a mutating mode, and commands only as
// host-named catalogue entries whose executable this invocation's contract
// requires. The operator catalogue is a ceiling, never a source of grants.
func (a *Adapter) grants(mode api.Mode, required []string) []api.Capability {
	grants := []api.Capability{
		{Handle: "candidate-read", Kind: api.CapabilityFileRead, Roots: []string{"."}},
		{Handle: "candidate-search", Kind: api.CapabilityFileSearch, Roots: []string{"."}},
	}
	if mode != api.ModeReadWrite {
		return grants
	}
	grants = append(grants, api.Capability{Handle: "candidate-write", Kind: api.CapabilityFileWrite, Roots: []string{"."}})
	if a.commands == nil {
		return grants
	}
	var commands []api.CommandGrant
	for _, spec := range a.catalogue {
		if slices.Contains(required, spec.Argv[0]) {
			commands = append(commands, api.CommandGrant{
				Name: spec.Name, Argv: spec.Argv, TimeoutSeconds: int(spec.Timeout / time.Second),
			})
		}
	}
	if len(commands) > 0 {
		grants = append(grants, api.Capability{Handle: "candidate-commands", Kind: api.CapabilityCommand, Commands: commands})
	}
	return grants
}

// hostContext is the host-trusted half of the context: only fields the
// runtime or the operator authored. Candidate-derived text never lands here.
func hostContext(req execution.Request) []api.ContextItem {
	var items []api.ContextItem
	add := func(prefix string, kind api.ContextKind, lines ...string) {
		for i, line := range lines {
			if strings.TrimSpace(line) != "" {
				items = append(items, contextItem(fmt.Sprintf("%s-%d", prefix, i), kind, api.TrustHost, line, ""))
			}
		}
	}
	add("trusted-instructions", api.ContextInstruction, req.TrustedInstructions)
	add("instruction", api.ContextInstruction, req.Instructions...)
	if req.Purpose != "" {
		add("purpose", api.ContextInstruction, "Invocation purpose: "+string(req.Purpose))
	}
	if req.Contract.ID != "" {
		items = append(items, contextItem("contract", api.ContextConstraint, api.TrustHost,
			"Engineering contract "+req.Contract.ID+" revision "+req.Contract.Revision, req.Contract.Revision))
	}
	add("acceptance", api.ContextConstraint, req.AcceptanceObligations...)
	add("constraint", api.ContextConstraint, req.Constraints...)
	add("prohibition", api.ContextConstraint, req.Prohibitions...)
	add("permission", api.ContextConstraint, req.Permissions...)
	return items
}

// untrustedContext is everything a verifier, a reviewer or an upstream worker
// wrote. It is required (remediation without its findings, or a review
// without the change, is not the work the host asked for) and never host
// trust, so the kernel frames it as data and never as an instruction.
func untrustedContext(req execution.Request) []api.ContextItem {
	var items []api.ContextItem
	for i, f := range req.Findings {
		items = append(items, contextItem(fmt.Sprintf("finding-%d", i), api.ContextObservation, api.TrustToolOutput,
			f.String()+"\n"+f.Diagnostic, ""))
	}
	for i, f := range req.Feedback {
		content := fmt.Sprintf("feedback key=%s class=%s actor=%s path=%s commit=%s\n%s", f.Key, f.Class, f.Actor, f.Path, f.Commit, f.Body)
		items = append(items, contextItem(fmt.Sprintf("feedback-%d", i), api.ContextObservation, api.TrustWorkspace, content, f.Commit))
	}
	for i, u := range req.Upstream {
		items = append(items, contextItem(fmt.Sprintf("upstream-%d", i), api.ContextObservation, api.TrustModel, upstreamText(u), u.Commit))
	}
	return items
}

func upstreamText(u execution.UpstreamContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "upstream stage=%s run=%s commit=%s tree=%s truncated=%t\n", u.StageID, u.RunID, u.Commit, u.Tree, u.Truncated)
	if h := u.Handoff; h != nil {
		fmt.Fprintf(&b, "handoff %s outcome=%s\n%s\nunresolved: %s\nrecommended next: %s\n",
			h.ID, h.Outcome, h.Summary, strings.Join(h.Unresolved, "; "), strings.Join(h.RecommendedNext, "; "))
	}
	b.WriteString(u.Diff)
	return b.String()
}

func contextItem(id string, kind api.ContextKind, trust api.Trust, content, revision string) api.ContextItem {
	return api.ContextItem{
		ID: id, Kind: kind, Trust: trust, Required: true,
		Content: content, ContentDigest: api.Digest([]byte(content)), Revision: revision,
	}
}

// instructionDigest binds the host-trusted instruction set in provenance.
func instructionDigest(host []api.ContextItem) (string, error) {
	data, err := json.Marshal(host)
	if err != nil {
		return "", fmt.Errorf("agentkernel: encode host instructions: %w", err)
	}
	return api.Digest(data), nil
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func wallInstant(now time.Time, limit time.Duration) time.Time {
	if limit <= 0 {
		return time.Time{}
	}
	return now.Add(limit)
}
