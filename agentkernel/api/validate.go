package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

// MaxObjectiveBytes bounds the objective text.
const MaxObjectiveBytes = 64 << 10

// MaxProviderRetries bounds the request-level retry allowance a host may grant.
const MaxProviderRetries = 10

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	digestPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	revisionPattern   = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	currencyPattern   = regexp.MustCompile(`^[A-Z]{3}$`)
)

// ValidationError is a refusal raised before any side effect.
type ValidationError struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Reason }

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// Digest returns the canonical content digest of data.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ValidIdentifier reports whether s is an acceptable opaque identifier.
func ValidIdentifier(s string) bool { return identifierPattern.MatchString(s) }

// ValidDigest reports whether s is a canonical sha256 digest.
func ValidDigest(s string) bool { return digestPattern.MatchString(s) }

// DecodeRequest strictly decodes a canonical JSON request: unknown fields,
// duplicate keys and trailing data are refused.
func DecodeRequest(data []byte) (ExecutionRequest, error) {
	var r ExecutionRequest
	if err := decodeStrict(data, &r); err != nil {
		return ExecutionRequest{}, err
	}
	return r, nil
}

func decodeStrict(data []byte, v any) error {
	if err := rejectDuplicateKeys(data); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("$", "decode: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return invalid("$", "trailing data after JSON value")
	}
	return nil
}

// rejectDuplicateKeys walks the token stream because encoding/json silently
// keeps the last of two duplicate keys, which would let a conflicting field
// hide behind an earlier one.
func rejectDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	type frame struct {
		keys     map[string]bool
		isObject bool
		wantKey  bool
	}
	var stack []*frame
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return invalid("$", "decode: %v", err)
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				if top != nil && top.isObject {
					top.wantKey = true
				}
				stack = append(stack, &frame{keys: map[string]bool{}, isObject: d == '{', wantKey: d == '{'})
			default:
				stack = stack[:len(stack)-1]
			}
			continue
		}
		if top == nil || !top.isObject {
			continue
		}
		if !top.wantKey {
			top.wantKey = true
			continue
		}
		key := tok.(string)
		if top.keys[key] {
			return invalid(key, "duplicate key")
		}
		top.keys[key] = true
		top.wantKey = false
	}
}

// Validate refuses a request that is malformed, internally conflicting, asks
// for an unsupported feature, or carries a bound the kernel cannot enforce.
// It performs no I/O and must run before any side effect.
func (r ExecutionRequest) Validate(now time.Time) error {
	if r.Version != ExecutionVersion {
		return invalid("version", "unsupported version %q", r.Version)
	}
	for field, id := range map[string]string{
		"execution_id": r.ExecutionID, "attempt_id": r.AttemptID, "workspace.id": r.Workspace.ID,
	} {
		if !ValidIdentifier(id) {
			return invalid(field, "invalid identifier %q", id)
		}
	}
	if strings.TrimSpace(r.Objective) == "" || len(r.Objective) > MaxObjectiveBytes {
		return invalid("objective", "must be non-empty and at most %d bytes", MaxObjectiveBytes)
	}
	if r.Mode != ModeReadOnly && r.Mode != ModeReadWrite {
		return invalid("mode", "unsupported mode %q", r.Mode)
	}
	checks := []func() error{
		r.Workspace.validate,
		r.Constraints.validate,
		func() error { return validateContext(r.Context) },
		func() error { return validateGrants(r.Grants, r.Mode) },
		func() error { return r.Budget.validate(now) },
		func() error { return validateProviders(r.Providers, r.Budget.Money) },
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

func (w WorkspaceRef) validate() error {
	if !ValidDigest(w.ManifestDigest) {
		return invalid("workspace.manifest_digest", "must be a sha256 digest")
	}
	if w.GitRevision != "" && !revisionPattern.MatchString(w.GitRevision) {
		return invalid("workspace.git_revision", "must be a full hex object name")
	}
	return nil
}

func (c Constraints) validate() error {
	if !ValidDigest(c.InstructionDigest) {
		return invalid("constraints.instruction_digest", "must be a sha256 digest")
	}
	seen := map[string]bool{}
	for _, f := range c.RequiredFeatures {
		if seen[f] {
			return invalid("constraints.required_features", "duplicate feature %q", f)
		}
		seen[f] = true
		if !slices.Contains(SupportedFeatures, f) {
			return invalid("constraints.required_features", "unsupported feature %q", f)
		}
	}
	return nil
}

var validKinds = []ContextKind{
	ContextInstruction, ContextConstraint, ContextTask, ContextObservation, ContextSourceCode,
	ContextDependency, ContextTest, ContextMemory, ContextBackground,
}

var validTrust = []Trust{TrustHost, TrustWorkspace, TrustToolOutput, TrustModel, TrustMemory}

func validateContext(items []ContextItem) error {
	seen := map[string]bool{}
	for i, it := range items {
		field := fmt.Sprintf("context[%d]", i)
		if err := it.Validate(); err != nil {
			return invalid(field, "%v", err)
		}
		if seen[it.ID] {
			return invalid(field, "duplicate id %q", it.ID)
		}
		seen[it.ID] = true
	}
	return nil
}

// Validate checks one item's identity, classification and trust. Instructions
// and constraints are host-only; no other origin can be elevated to them.
func (it ContextItem) Validate() error {
	if !ValidIdentifier(it.ID) {
		return fmt.Errorf("invalid id %q", it.ID)
	}
	if !slices.Contains(validKinds, it.Kind) {
		return fmt.Errorf("unknown kind %q", it.Kind)
	}
	if !slices.Contains(validTrust, it.Trust) {
		return fmt.Errorf("unknown trust %q", it.Trust)
	}
	hostOnly := it.Kind == ContextInstruction || it.Kind == ContextConstraint
	if hostOnly && it.Trust != TrustHost {
		return fmt.Errorf("kind %q requires host trust, got %q", it.Kind, it.Trust)
	}
	if it.ContentDigest != Digest([]byte(it.Content)) {
		return fmt.Errorf("content_digest does not match content")
	}
	return nil
}

var validCapabilities = []CapabilityKind{
	CapabilityFileRead, CapabilityFileSearch, CapabilityFileWrite, CapabilityCommand,
}

func validateGrants(grants []Capability, mode Mode) error {
	handles := map[string]bool{}
	for i, g := range grants {
		field := fmt.Sprintf("grants[%d]", i)
		if !ValidIdentifier(g.Handle) {
			return invalid(field, "invalid handle %q", g.Handle)
		}
		if handles[g.Handle] {
			return invalid(field, "duplicate handle %q", g.Handle)
		}
		handles[g.Handle] = true
		if !slices.Contains(validCapabilities, g.Kind) {
			return invalid(field, "unknown kind %q", g.Kind)
		}
		mutating := g.Kind == CapabilityFileWrite || g.Kind == CapabilityCommand
		if mode == ModeReadOnly && mutating {
			return invalid(field, "kind %q is not grantable in read_only mode", g.Kind)
		}
		if err := g.validateShape(); err != nil {
			return invalid(field, "%v", err)
		}
	}
	return nil
}

func (g Capability) validateShape() error {
	if g.Kind == CapabilityCommand {
		if len(g.Roots) > 0 || len(g.Commands) == 0 {
			return fmt.Errorf("command.run takes commands and no roots")
		}
		names := map[string]bool{}
		for _, c := range g.Commands {
			if !ValidIdentifier(c.Name) || names[c.Name] || len(c.Argv) == 0 || c.TimeoutSeconds <= 0 {
				return fmt.Errorf("command %q needs a unique name, argv and positive timeout", c.Name)
			}
			names[c.Name] = true
		}
		return nil
	}
	if len(g.Commands) > 0 || len(g.Roots) == 0 {
		return fmt.Errorf("%s takes roots and no commands", g.Kind)
	}
	for _, root := range g.Roots {
		if !ValidRelativePath(root) {
			return fmt.Errorf("root %q must be a clean workspace-relative path", root)
		}
	}
	return nil
}

// ValidRelativePath reports whether p is a clean, slash-separated path inside
// the workspace: not absolute, no "..", no backslash, drive letter or NUL.
func ValidRelativePath(p string) bool {
	if p == "" || strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "/") {
		return false
	}
	if len(p) >= 2 && p[1] == ':' {
		return false
	}
	if path.Clean(p) != p {
		return false
	}
	return p == "." || (p != ".." && !strings.HasPrefix(p, "../"))
}

func (b Budget) validate(now time.Time) error {
	if b.Deadline.IsZero() || !b.Deadline.After(now) {
		return invalid("budget.deadline", "must be in the future")
	}
	positives := map[string]int64{
		"budget.max_iterations": int64(b.MaxIterations), "budget.max_tool_calls": int64(b.MaxToolCalls),
		"budget.max_input_tokens": b.MaxInputTokens, "budget.max_output_tokens": b.MaxOutputTokens,
		"budget.max_artifact_bytes": b.MaxArtifactBytes,
	}
	for field, v := range positives {
		if v <= 0 {
			return invalid(field, "must be positive")
		}
	}
	if b.MaxProviderRetries < 0 || b.MaxProviderRetries > MaxProviderRetries {
		return invalid("budget.max_provider_retries", "must be within 0..%d", MaxProviderRetries)
	}
	if b.Money != nil && (!currencyPattern.MatchString(b.Money.Currency) || b.Money.MaxMicros <= 0) {
		return invalid("budget.money", "needs an ISO currency and positive max_micros")
	}
	return nil
}

func validateProviders(bindings []ProviderBinding, money *MoneyCeiling) error {
	if len(bindings) == 0 {
		return invalid("providers", "at least one binding is required")
	}
	ids := map[string]bool{}
	pinned := 0
	for i, p := range bindings {
		field := fmt.Sprintf("providers[%d]", i)
		if ids[p.ID] {
			return invalid(field, "duplicate id %q", p.ID)
		}
		ids[p.ID] = true
		if err := p.validate(); err != nil {
			return invalid(field, "%v", err)
		}
		if p.Pinned {
			pinned++
		}
		if money != nil && p.Eligible && !p.Pricing.prices(money.Currency) {
			return invalid(field, "money ceiling is unenforceable without trusted %s pricing", money.Currency)
		}
	}
	if pinned > 1 {
		return invalid("providers", "at most one binding may be pinned")
	}
	return nil
}

func (p ProviderBinding) validate() error {
	if !ValidIdentifier(p.ID) || !ValidIdentifier(p.Kind) {
		return fmt.Errorf("invalid id or kind")
	}
	if p.Model == "" || p.ModelVersion == "" || p.ConfigFingerprint == "" {
		return fmt.Errorf("model, model_version (or %q) and config_fingerprint are required", UnknownVersion)
	}
	if p.Isolation != IsolationUnproven && p.Isolation != IsolationHostProven {
		return fmt.Errorf("unknown isolation %q", p.Isolation)
	}
	if p.ContextWindow <= 0 || p.MaxOutputTokens <= 0 {
		return fmt.Errorf("context_window and max_output_tokens must be positive")
	}
	if p.Pinned && !p.Eligible {
		return fmt.Errorf("a pinned binding must be eligible")
	}
	if p.CredentialHandle != "" && !ValidIdentifier(p.CredentialHandle) {
		return fmt.Errorf("invalid credential_handle")
	}
	if p.Pricing != nil && !p.Pricing.prices(p.Pricing.Currency) {
		return fmt.Errorf("pricing needs currency, non-negative rates, source and version")
	}
	return nil
}

// prices reports whether p is a complete trusted rate card in currency.
func (p *Pricing) prices(currency string) bool {
	if p == nil || p.Currency != currency || !currencyPattern.MatchString(currency) {
		return false
	}
	if p.InputMicrosPerMillion < 0 || p.OutputMicrosPerMillion < 0 || p.CachedInputMicrosPerMillion < 0 {
		return false
	}
	return p.Source != "" && p.Version != ""
}
