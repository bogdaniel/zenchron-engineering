package runtime

import (
	"errors"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// ExecutionAttemptProvenance is the execution.attempt_provenance payload: the
// invocation provenance of ONE physical provider attempt, journalled once and
// never rewritten (#327).
//
// It is a dedicated event rather than a member of operation.after so it does
// not compete with the scheduler document and the operation result for one
// payload ceiling, and because it is attempt-scoped where operation.after is
// operation-scoped: two attempts of one operation are two events keyed by
// AttemptIdentity, never one record overwritten.
//
// The #241 Git refusals are not carried. They are already folded into the
// operation result as a count and the latest shape, and a second copy here
// would be a second source for the same fact.
type ExecutionAttemptProvenance struct {
	OperationID     string               `json:"operation_id"`
	AttemptIdentity int                  `json:"attempt_identity"`
	Invocation      InvocationProvenance `json:"invocation"`
}

// argvTruncated marks a vector that was cut short, so a bounded argv is never
// mistaken for the whole one.
const argvTruncated = "[argv truncated]"

// newExecutionAttemptProvenance builds the event for one attempt. It is TOTAL:
// whatever the provider reported, the record it returns is appendable, because
// a record refused at append would fail the pass before operation.after and
// drop exactly the attempt that most needed explaining.
//
// Every field is already bounded where it is produced. What no per-field bound
// can promise is the SUM, so the observation is fitted (fitObservation) and, if
// even the identity strings around it cannot be recorded, they are reduced to
// identifiers too.
func newExecutionAttemptProvenance(operationID string, attempt int, invocation InvocationProvenance) ExecutionAttemptProvenance {
	invocation.GitRefusals = nil
	record := ExecutionAttemptProvenance{OperationID: operationID, AttemptIdentity: attempt, Invocation: invocation}
	record.Invocation.InvocationObservation = fitObservation(invocation.InvocationObservation, func(o domain.InvocationObservation) bool {
		candidate := record
		candidate.Invocation.InvocationObservation = o
		return appendable(EventExecutionAttemptProvenance, candidate)
	})
	if appendable(EventExecutionAttemptProvenance, record) {
		return record
	}
	// THE LAST RESORT: identity reduced to what is an identifier, the
	// observation to its fixed-size core. Nothing here depends on provider or
	// configuration text, so it always fits.
	identifier := func(s string) string {
		if domain.IsInvocationIdentifier(s) {
			return s
		}
		return ""
	}
	minimal := InvocationProvenance{
		AgentID: identifier(invocation.AgentID), ProviderKind: identifier(invocation.ProviderKind),
		TrustMode: TrustMode(identifier(string(invocation.TrustMode))), Model: identifier(invocation.Model),
		DeadlineBound:         invocation.DeadlineBound,
		InvocationObservation: domain.MinimalInvocationObservation(invocation.InvocationObservation),
	}
	return ExecutionAttemptProvenance{OperationID: boundedDetail(operationID), AttemptIdentity: max(1, attempt), Invocation: minimal}
}

// fitObservation bounds one observation so that fits holds: the argv tail is
// trimmed first, visibly, because argv is the one member long enough to matter
// and the one whose tail explains least; if that is not enough, the fixed-size
// minimal record is returned instead, marked provenance_truncated.
func fitObservation(o domain.InvocationObservation, fits func(domain.InvocationObservation) bool) domain.InvocationObservation {
	o.Argv = append([]string(nil), o.Argv...)
	for len(o.Argv) > 0 && !fits(o) {
		argv := o.Argv
		if argv[len(argv)-1] == argvTruncated {
			argv = argv[:len(argv)-1]
		}
		if len(argv) == 0 {
			o.Argv = nil
			break
		}
		o.Argv = append(argv[:len(argv)-1], argvTruncated)
	}
	if fits(o) {
		return o
	}
	return domain.MinimalInvocationObservation(o)
}

// appendable reports whether a payload would pass the journal's own append
// validation - the canonical ceiling and the type's schema - which is the only
// definition of "fits" that cannot disagree with the append.
func appendable(eventType string, payload any) bool {
	raw, err := marshalPayloadJSON(payload)
	return err == nil && validateEventPayload(EngineeringEvent{Type: eventType, Payload: raw}) == nil
}

// validate is the event's schema. It refuses what the producer never writes -
// an unbound attempt, a carried refusal list, an unbounded denied-tool set -
// so a malformed record cannot reach the journal in a readable-looking form.
func (p ExecutionAttemptProvenance) validate() error {
	if p.AttemptIdentity < 1 {
		return fmt.Errorf("attempt provenance must name the physical attempt it describes, got %d", p.AttemptIdentity)
	}
	inv := p.Invocation
	if len(inv.GitRefusals) > 0 {
		return errors.New("attempt provenance carries no git refusals; they are counted on the operation result")
	}
	errs := []error{required("operation_id", p.OperationID), validateInvocationObservation(inv.InvocationObservation)}
	switch inv.DeadlineBound {
	case "", BoundAttemptWall, BoundRunActiveWork:
	default:
		errs = append(errs, fmt.Errorf("invocation deadline_bound %q is not a deadline bound", inv.DeadlineBound))
	}
	for name, value := range map[string]string{
		"agent_id": inv.AgentID, "provider_kind": inv.ProviderKind, "trust_mode": string(inv.TrustMode), "model": inv.Model,
	} {
		errs = append(errs, bounded("invocation."+name, value))
	}
	return errors.Join(errs...)
}

// validateInvocationObservation is the bound every durable copy of an
// observation is held to, on the run journal and on the plan journal alike.
func validateInvocationObservation(o domain.InvocationObservation) error {
	if len(o.Argv) > maxProvenanceArgs+1 {
		return fmt.Errorf("invocation argv has %d elements, above the %d element bound", len(o.Argv), maxProvenanceArgs+1)
	}
	if len(o.PermissionDeniedTools) > domain.MaxPermissionDeniedTools {
		return fmt.Errorf("invocation names %d denied tools, above the bound of %d", len(o.PermissionDeniedTools), domain.MaxPermissionDeniedTools)
	}
	if len(o.ProviderEnvironment) > domain.MaxProviderEnvironment {
		return fmt.Errorf("invocation records %d environment variables, above the bound of %d", len(o.ProviderEnvironment), domain.MaxProviderEnvironment)
	}
	var errs []error
	for _, entry := range o.ProviderEnvironment {
		if !recordedEnvName(entry.Name) || credentialShapedName(entry.Name) {
			errs = append(errs, fmt.Errorf("invocation environment name %q is not on the recorded allowlist", entry.Name))
		}
		if entry.Value != nil {
			errs = append(errs, bounded("invocation.provider_environment[].value", *entry.Value))
		}
		switch {
		case entry.SHA256 != "" && !isSHA256Hex(entry.SHA256):
			errs = append(errs, fmt.Errorf("invocation environment %s identity is not a SHA-256", entry.Name))
		case entry.SHA256 != "" && entry.Value != nil:
			errs = append(errs, fmt.Errorf("invocation environment %s carries both a value and an identity", entry.Name))
		case entry.Value != nil && hashedEnvName(entry.Name):
			errs = append(errs, fmt.Errorf("invocation environment %s is a host path recorded only by identity, never literally", entry.Name))
		case entry.SHA256 != "" && !hashedEnvName(entry.Name):
			errs = append(errs, fmt.Errorf("invocation environment %s is recorded literally, never by identity", entry.Name))
		}
	}
	for n, arg := range o.Argv {
		errs = append(errs, bounded("invocation.argv[]", arg))
		// Recorded argv names runtime-owned paths by role (#464); a raw host
		// path reaching the journal is a projection defect, refused here.
		if isHostPath(arg) {
			errs = append(errs, fmt.Errorf("invocation argv[%d] is a raw host path; record its logical reference", n))
		}
	}
	for _, tool := range o.PermissionDeniedTools {
		if !domain.IsInvocationIdentifier(tool) {
			errs = append(errs, fmt.Errorf("invocation denied tool %q is not a tool identifier", tool))
		}
	}
	for name, value := range map[string]string{
		"executable": o.Executable, "provider_version": o.Version, "sandbox_mode": o.SandboxMode,
		"permission_mode": o.PermissionMode, "auth_mode": o.AuthMode, "auth_mode_source": o.AuthModeSource,
		"prompt_sha256": o.PromptSHA256, "termination_cause": o.TerminationCause, "progress_mode": o.ProgressMode,
	} {
		errs = append(errs, bounded("invocation."+name, value))
	}
	return errors.Join(errs...)
}
