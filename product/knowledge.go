package product

// Knowledge promotion (#476): an agent's discovery is recorded at EXECUTION or
// WORK_UNIT scope and stays there until an authorized, provenance-preserving
// Promote call moves it up the ladder (scope.go). There is no path that lets a
// directly authored entry land at feature/product/organization scope, and no
// path that promotes without an explicit authorizer: those two refusals are
// what makes "agent discovery never becomes organization truth automatically"
// true by construction rather than by convention.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// KnowledgeSchemaVersion versions both durable documents in this file.
const KnowledgeSchemaVersion = "0.1"

const (
	maxKnowledgeEntryIDBytes   = 80
	maxKnowledgeStatementBytes = 4 << 10
	maxKnowledgeProducerBytes  = 200
)

// ProvenanceType is how a knowledge entry came to exist.
type ProvenanceType string

const (
	ProvenanceAgentDiscovery ProvenanceType = "agent_discovery"
	ProvenanceHumanAuthored  ProvenanceType = "human_authored"
	// ProvenancePromotion marks an entry Promote produced. Only Promote may
	// use it; NewKnowledgeEntry refuses it.
	ProvenancePromotion ProvenanceType = "promotion"
)

func knownProvenanceType(t ProvenanceType) bool {
	switch t {
	case ProvenanceAgentDiscovery, ProvenanceHumanAuthored, ProvenancePromotion:
		return true
	default:
		return false
	}
}

// Provenance identifies who or what produced a knowledge entry.
type Provenance struct {
	Type     ProvenanceType `json:"type"`
	Producer string         `json:"producer"`
}

// KnowledgeEntry is one claim, scoped on the #476 ladder and attributed to its
// provenance. It is immutable and content-identified: two calls that construct
// the same entry at the same instant are the same entry.
type KnowledgeEntry struct {
	SchemaVersion string     `json:"schema_version"`
	ID            string     `json:"id"`
	ProductID     string     `json:"product_id"`
	Scope         Scope      `json:"scope"`
	Statement     string     `json:"statement"`
	Provenance    Provenance `json:"provenance"`
	// PromotedFrom is the id of the entry this one was promoted from. It is
	// set if and only if Provenance.Type is ProvenancePromotion.
	PromotedFrom *string   `json:"promoted_from,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// NewKnowledgeEntry authors a claim directly, at execution or work_unit scope
// only. A caller that wants it recorded wider must call Promote.
func NewKnowledgeEntry(productID string, scope Scope, statement string, provenance Provenance, at time.Time) (KnowledgeEntry, error) {
	if provenance.Type == ProvenancePromotion {
		return KnowledgeEntry{}, errors.New("a promoted knowledge entry is produced by Promote, not authored directly")
	}
	direction, err := compareScope(scope, ScopeWorkUnit)
	if err != nil {
		return KnowledgeEntry{}, err
	}
	if direction > 0 {
		return KnowledgeEntry{}, fmt.Errorf(
			"knowledge may only be authored directly at %q or %q scope; %q is reached only through promotion",
			ScopeExecution, ScopeWorkUnit, scope)
	}
	entry := KnowledgeEntry{
		SchemaVersion: KnowledgeSchemaVersion, ProductID: productID, Scope: scope,
		Statement: statement, Provenance: provenance, CreatedAt: at,
	}
	id, err := entry.contentID()
	if err != nil {
		return KnowledgeEntry{}, err
	}
	entry.ID = id
	if err := entry.Validate(); err != nil {
		return KnowledgeEntry{}, err
	}
	return entry, nil
}

// Promote is the ONLY way a claim reaches feature, product or organization
// scope. AuthorizedBy is required and never inferred: a missing authorizer is
// refused rather than defaulted to the entry's own producer, which is exactly
// the unauthorized scope widening #476's acceptance refuses.
func Promote(source KnowledgeEntry, targetScope Scope, authorizedBy string, at time.Time) (KnowledgeEntry, KnowledgePromotion, error) {
	if strings.TrimSpace(authorizedBy) == "" {
		return KnowledgeEntry{}, KnowledgePromotion{}, errors.New(
			"promotion requires an explicit authorizer; a claim never widens its own scope")
	}
	if err := source.Validate(); err != nil {
		return KnowledgeEntry{}, KnowledgePromotion{}, fmt.Errorf("source knowledge entry is invalid: %w", err)
	}
	direction, err := compareScope(source.Scope, targetScope)
	if err != nil {
		return KnowledgeEntry{}, KnowledgePromotion{}, err
	}
	if direction >= 0 {
		return KnowledgeEntry{}, KnowledgePromotion{}, fmt.Errorf(
			"promotion moves up the scope ladder; %q is not above %q", targetScope, source.Scope)
	}
	promoted := KnowledgeEntry{
		SchemaVersion: KnowledgeSchemaVersion, ProductID: source.ProductID, Scope: targetScope,
		Statement:    source.Statement,
		Provenance:   Provenance{Type: ProvenancePromotion, Producer: authorizedBy},
		PromotedFrom: &source.ID, CreatedAt: at,
	}
	id, err := promoted.contentID()
	if err != nil {
		return KnowledgeEntry{}, KnowledgePromotion{}, err
	}
	promoted.ID = id
	if err := promoted.Validate(); err != nil {
		return KnowledgeEntry{}, KnowledgePromotion{}, err
	}
	record := KnowledgePromotion{
		SchemaVersion: KnowledgeSchemaVersion, SourceEntryID: source.ID, SourceScope: source.Scope,
		SourceProvenance: source.Provenance, TargetScope: targetScope, AuthorizedBy: authorizedBy,
		PromotedEntryID: promoted.ID, PromotedAt: at,
	}
	recordID, err := record.contentID()
	if err != nil {
		return KnowledgeEntry{}, KnowledgePromotion{}, err
	}
	record.ID = recordID
	if err := record.Validate(); err != nil {
		return KnowledgeEntry{}, KnowledgePromotion{}, err
	}
	return promoted, record, nil
}

// Validate is the complete deterministic admission check for one entry,
// independent of any store.
func (e KnowledgeEntry) Validate() error {
	if e.SchemaVersion != KnowledgeSchemaVersion {
		return fmt.Errorf("knowledge entry schema version %q is not %q", e.SchemaVersion, KnowledgeSchemaVersion)
	}
	if err := boundedField("knowledge entry id", e.ID, maxKnowledgeEntryIDBytes); err != nil {
		return err
	}
	if err := boundedField("knowledge entry product id", e.ProductID, maxProductNameBytes); err != nil {
		return err
	}
	if !KnownScope(e.Scope) {
		return fmt.Errorf("knowledge entry scope %q is not a known scope", e.Scope)
	}
	if err := boundedField("knowledge statement", e.Statement, maxKnowledgeStatementBytes); err != nil {
		return err
	}
	if !knownProvenanceType(e.Provenance.Type) {
		return fmt.Errorf("knowledge entry provenance type %q is not known", e.Provenance.Type)
	}
	if err := boundedField("knowledge provenance producer", e.Provenance.Producer, maxKnowledgeProducerBytes); err != nil {
		return err
	}
	if e.CreatedAt.IsZero() {
		return errors.New("knowledge entry creation time is required")
	}
	switch e.Provenance.Type {
	case ProvenancePromotion:
		if e.PromotedFrom == nil || strings.TrimSpace(*e.PromotedFrom) == "" {
			return errors.New("a promoted knowledge entry needs the source entry id it was promoted from")
		}
	default:
		if e.PromotedFrom != nil {
			return fmt.Errorf("knowledge entry provenance %q may not carry a promoted-from source", e.Provenance.Type)
		}
	}
	return nil
}

func (e KnowledgeEntry) contentID() (string, error) {
	digest, err := domain.Digest(struct {
		ProductID    string     `json:"product_id"`
		Scope        Scope      `json:"scope"`
		Statement    string     `json:"statement"`
		Provenance   Provenance `json:"provenance"`
		PromotedFrom *string    `json:"promoted_from,omitempty"`
		CreatedAt    string     `json:"created_at"`
	}{e.ProductID, e.Scope, e.Statement, e.Provenance, e.PromotedFrom, e.CreatedAt.UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return "", err
	}
	return "knowledge-" + digest[:32], nil
}

// KnowledgePromotion is the durable, auditable record of one Promote call. It
// carries the source's full provenance forward, so a reader of an organization-
// scope entry can always trace it back to the agent discovery it started as.
type KnowledgePromotion struct {
	SchemaVersion    string     `json:"schema_version"`
	ID               string     `json:"id"`
	SourceEntryID    string     `json:"source_entry_id"`
	SourceScope      Scope      `json:"source_scope"`
	SourceProvenance Provenance `json:"source_provenance"`
	TargetScope      Scope      `json:"target_scope"`
	AuthorizedBy     string     `json:"authorized_by"`
	PromotedEntryID  string     `json:"promoted_entry_id"`
	PromotedAt       time.Time  `json:"promoted_at"`
}

// Validate is the complete deterministic admission check for one promotion
// record, independent of any store.
func (r KnowledgePromotion) Validate() error {
	if r.SchemaVersion != KnowledgeSchemaVersion {
		return fmt.Errorf("knowledge promotion schema version %q is not %q", r.SchemaVersion, KnowledgeSchemaVersion)
	}
	if err := boundedField("promotion id", r.ID, maxKnowledgeEntryIDBytes); err != nil {
		return err
	}
	if err := boundedField("promotion source entry id", r.SourceEntryID, maxKnowledgeEntryIDBytes); err != nil {
		return err
	}
	if err := boundedField("promotion promoted entry id", r.PromotedEntryID, maxKnowledgeEntryIDBytes); err != nil {
		return err
	}
	if err := boundedField("promotion authorizer", r.AuthorizedBy, maxKnowledgeProducerBytes); err != nil {
		return err
	}
	if !knownProvenanceType(r.SourceProvenance.Type) {
		return fmt.Errorf("promotion source provenance type %q is not known", r.SourceProvenance.Type)
	}
	if !KnownScope(r.SourceScope) {
		return fmt.Errorf("promotion source scope %q is not a known scope", r.SourceScope)
	}
	if !KnownScope(r.TargetScope) {
		return fmt.Errorf("promotion target scope %q is not a known scope", r.TargetScope)
	}
	direction, err := compareScope(r.SourceScope, r.TargetScope)
	if err != nil {
		return err
	}
	if direction >= 0 {
		return fmt.Errorf("promotion target scope %q is not above source scope %q", r.TargetScope, r.SourceScope)
	}
	if r.PromotedAt.IsZero() {
		return errors.New("promotion time is required")
	}
	return nil
}

func (r KnowledgePromotion) contentID() (string, error) {
	digest, err := domain.Digest(struct {
		SourceEntryID string `json:"source_entry_id"`
		TargetScope   Scope  `json:"target_scope"`
		AuthorizedBy  string `json:"authorized_by"`
		PromotedAt    string `json:"promoted_at"`
	}{r.SourceEntryID, r.TargetScope, r.AuthorizedBy, r.PromotedAt.UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return "", err
	}
	return "promotion-" + digest[:32], nil
}
