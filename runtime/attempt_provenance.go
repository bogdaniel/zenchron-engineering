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

// newExecutionAttemptProvenance builds the event for one attempt, bounded to
// the canonical payload ceiling.
//
// Every field is already bounded where it is produced. What no per-field
// bound can promise is the SUM - 33 argv elements at the element bound plus
// long absolute paths elsewhere exceed 8 KiB - and a record refused at append
// would be a record dropped. So the argv tail is trimmed, and visibly marked,
// until the whole payload fits: argv is the one member long enough to matter
// and the one whose tail explains least.
func newExecutionAttemptProvenance(operationID string, attempt int, invocation InvocationProvenance) ExecutionAttemptProvenance {
	invocation.GitRefusals = nil
	invocation.Argv = append([]string(nil), invocation.Argv...)
	record := ExecutionAttemptProvenance{OperationID: operationID, AttemptIdentity: attempt, Invocation: invocation}
	for len(record.Invocation.Argv) > 0 && !fitsPayloadCeiling(record) {
		argv := record.Invocation.Argv
		if argv[len(argv)-1] == argvTruncated {
			argv = argv[:len(argv)-1]
		}
		if len(argv) == 0 {
			record.Invocation.Argv = nil
			break
		}
		record.Invocation.Argv = append(argv[:len(argv)-1], argvTruncated)
	}
	return record
}

func fitsPayloadCeiling(v any) bool {
	canonical, err := CanonicalJSON(v)
	return err == nil && len(canonical) <= maxCanonicalPayloadBytes
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
	if len(inv.Argv) > maxProvenanceArgs+1 {
		return fmt.Errorf("attempt provenance argv has %d elements, above the %d element bound", len(inv.Argv), maxProvenanceArgs+1)
	}
	if len(inv.PermissionDeniedTools) > domain.MaxPermissionDeniedTools {
		return fmt.Errorf("attempt provenance names %d denied tools, above the bound of %d", len(inv.PermissionDeniedTools), domain.MaxPermissionDeniedTools)
	}
	errs := []error{required("operation_id", p.OperationID)}
	for _, arg := range inv.Argv {
		errs = append(errs, bounded("invocation.argv[]", arg))
	}
	for _, tool := range inv.PermissionDeniedTools {
		if !validDeniedToolName(tool) {
			errs = append(errs, fmt.Errorf("attempt provenance denied tool %q is not a tool identifier", tool))
		}
	}
	for name, value := range map[string]string{
		"agent_id": inv.AgentID, "provider_kind": inv.ProviderKind, "trust_mode": string(inv.TrustMode), "model": inv.Model,
		"executable": inv.Executable, "provider_version": inv.Version, "sandbox_mode": inv.SandboxMode,
		"permission_mode": inv.PermissionMode, "auth_mode": inv.AuthMode, "auth_mode_source": inv.AuthModeSource,
		"prompt_sha256": inv.PromptSHA256, "termination_cause": inv.TerminationCause, "progress_mode": inv.ProgressMode,
	} {
		errs = append(errs, bounded("invocation."+name, value))
	}
	return errors.Join(errs...)
}
