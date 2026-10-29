// Package memory keeps versioned, provenance-bearing derived records in
// partitions over storage.Records. A record is never authoritative: on
// retrieval it is untrusted memory content, and only records whose validity is
// currently "valid" ever surface as context.
package memory

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// RecordVersion is the only record format this package reads or writes.
const RecordVersion = "agentkernel.memory/v0.1"

// ItemPrefix is prepended to a record ID to form its context item ID, so a
// remembered item can never collide with a host-supplied item ID.
const ItemPrefix = "mem:"

// Kind separates what was deterministically observed from what a model
// inferred and from a task summary.
type Kind string

const (
	KindObservation Kind = "observation"
	KindInference   Kind = "inference"
	KindSummary     Kind = "summary"
)

// Validity is a record's current standing. Only valid records surface.
type Validity string

const (
	Valid      Validity = "valid"
	Stale      Validity = "stale"
	Conflicted Validity = "conflicted"
	Unknown    Validity = "unknown"
)

// Partition scopes records by repository, workspace or execution, and access
// scope. Reads require the exact partition; there is no cross-partition list.
type Partition struct {
	Repository string `json:"repository"`
	Workspace  string `json:"workspace"`
	Access     string `json:"access"`
}

// Derivation identifies how a record was produced. Its identity invalidates
// every record produced the same way when the method changes.
type Derivation struct {
	Method  string `json:"method"`
	Tool    string `json:"tool,omitempty"`
	Model   string `json:"model,omitempty"`
	Version string `json:"version"`
}

// Record is one remembered item. There is deliberately no authoritative field.
type Record struct {
	Version   string    `json:"version"`
	ID        string    `json:"id"`
	Kind      Kind      `json:"kind"`
	Partition Partition `json:"partition"`
	// Subject groups records about the same thing; two valid records with the
	// same subject and different content are both marked conflicted.
	Subject       string     `json:"subject,omitempty"`
	Content       string     `json:"content"`
	SourceDigests []string   `json:"source_digests"`
	Derivation    Derivation `json:"derivation"`
	CreatedAt     time.Time  `json:"created_at"`
	ValidatedAt   time.Time  `json:"validated_at,omitzero"`
	DependsOn     []string   `json:"depends_on,omitempty"`
	Validity      Validity   `json:"validity"`
	// Confidence is in [0,1]; nil means unknown, never zero or one.
	Confidence *float64 `json:"confidence"`
}

// ErrInvalidRecord reports a record refused before any write.
var ErrInvalidRecord = errors.New("memory: invalid record")

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRecord, fmt.Sprintf(format, args...))
}

func (p Partition) validate() error {
	for _, s := range []string{p.Repository, p.Workspace, p.Access} {
		if !api.ValidIdentifier(s) {
			return refuse("partition component %q is not a valid identifier", s)
		}
	}
	return nil
}

// Validate checks a record's shape and provenance.
func (r Record) Validate() error {
	if r.Version != RecordVersion {
		return refuse("unsupported version %q", r.Version)
	}
	if !api.ValidIdentifier(ItemPrefix + r.ID) {
		return refuse("invalid id %q", r.ID)
	}
	if !slices.Contains([]Kind{KindObservation, KindInference, KindSummary}, r.Kind) {
		return refuse("unknown kind %q", r.Kind)
	}
	if !slices.Contains([]Validity{Valid, Stale, Conflicted, Unknown}, r.Validity) {
		return refuse("unknown validity %q", r.Validity)
	}
	if err := r.Partition.validate(); err != nil {
		return err
	}
	if r.Subject != "" && !api.ValidIdentifier(r.Subject) {
		return refuse("invalid subject %q", r.Subject)
	}
	if r.Content == "" || r.CreatedAt.IsZero() {
		return refuse("content and created_at are required")
	}
	return r.validateProvenance()
}

func (r Record) validateProvenance() error {
	// Without a source a record could not be invalidated by source identity.
	if len(r.SourceDigests) == 0 {
		return refuse("at least one source digest is required")
	}
	for _, d := range r.SourceDigests {
		if !api.ValidDigest(d) {
			return refuse("invalid source digest %q", d)
		}
	}
	if r.Derivation.Method == "" || r.Derivation.Version == "" {
		return refuse("derivation method and version are required")
	}
	if r.Kind == KindInference && r.Derivation.Model == "" {
		return refuse("an inference must name the model that derived it")
	}
	for _, dep := range r.DependsOn {
		if dep == r.ID || !api.ValidIdentifier(ItemPrefix+dep) {
			return refuse("invalid dependency %q", dep)
		}
	}
	if r.Confidence != nil && !(*r.Confidence >= 0 && *r.Confidence <= 1) { // also refuses NaN
		return refuse("confidence must be within [0,1] or unknown")
	}
	return nil
}
