package product

// Knowledge promotion (#476): an agent's discovery is recorded at EXECUTION or
// WORK_UNIT scope, tagged with the EXACT owner (run/work unit) that produced
// it, and stays visible to only that owner until an authorized, provenance-
// preserving Promote call moves it up the ladder (scope.go) into scope-wide
// visibility with no owner of its own. Visibility is never "scope_rank <=
// requested": a raw, unpromoted discovery is visible to its own owner alone,
// at any scope a caller asks for; a wider-scope reader sees only entries that
// went through Promote. That separation - not merely the scope label - is
// what keeps "agent discovery never becomes organization truth automatically"
// true, and it is enforced again at the store boundary (runtime/product_store.go),
// which never trusts a caller-supplied id, scope or provenance without
// recomputing or cross-checking it.
//
// There is no path that lets a directly authored entry land at feature,
// product or organization scope, and no path that promotes without an
// explicit authorizer.

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
	maxKnowledgeOwnerRefBytes  = 200
	maxKnowledgeAudienceBytes  = 200
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
// the same entry at the same instant are the same entry, and Validate refuses
// any entry whose ID does not match that recomputed content - an ID is never
// trusted at face value, from a caller or from storage.
type KnowledgeEntry struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	ProductID     string `json:"product_id"`
	Scope         Scope  `json:"scope"`
	// OwnerRef is the exact execution or work unit this entry belongs to. It
	// is required when Scope is execution or work_unit - that is the whole
	// visibility rule for a raw discovery, enforced again by the store
	// (OwnedKnowledge only ever matches one exact owner_ref) - and it is
	// forbidden on a promoted entry: visibility above work_unit scope comes
	// from having been promoted, never from who owns it.
	OwnerRef string `json:"owner_ref,omitempty"`
	// AudienceRef is the intended TARGET of a promotion below product scope.
	// A scope rank alone is not an audience: promoting an execution claim to
	// work_unit scope must still name WHICH work unit it is for, or every
	// other work unit in the product could read it too. Required when a
	// promoted entry's Scope is work_unit or feature; forbidden at product or
	// organization scope, which carry no audience restriction, and forbidden
	// on a directly authored entry, which is never promoted in the first place.
	AudienceRef string     `json:"audience_ref,omitempty"`
	Statement   string     `json:"statement"`
	Provenance  Provenance `json:"provenance"`
	// PromotedFrom is the id of the entry this one was promoted from. It is
	// set if and only if Provenance.Type is ProvenancePromotion.
	PromotedFrom *string   `json:"promoted_from,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// NewKnowledgeEntry authors a claim directly, at execution or work_unit scope
// only, owned by exactly the caller's ownerRef. A caller that wants it
// recorded wider must call Promote.
func NewKnowledgeEntry(productID string, scope Scope, ownerRef, statement string, provenance Provenance, at time.Time) (KnowledgeEntry, error) {
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
		SchemaVersion: KnowledgeSchemaVersion, ProductID: productID, Scope: scope, OwnerRef: ownerRef,
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
//
// audienceRef names the intended target when targetScope is work_unit or
// feature - a scope rank is not an audience, so promoting "to work_unit
// scope" with no further identity would otherwise be readable by every work
// unit in the product. It is required there and forbidden at product or
// organization scope, which have no narrower audience to restrict to.
func Promote(source KnowledgeEntry, targetScope Scope, audienceRef, authorizedBy string, at time.Time) (KnowledgeEntry, KnowledgePromotion, error) {
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
		AudienceRef: audienceRef, Statement: source.Statement,
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
		SourceProvenance: source.Provenance, TargetScope: targetScope, AudienceRef: audienceRef,
		AuthorizedBy: authorizedBy, PromotedEntryID: promoted.ID, PromotedAt: at,
	}
	recordID, err := record.contentID()
	if err != nil {
		return KnowledgeEntry{}, KnowledgePromotion{}, err
	}
	record.ID = recordID
	if err := record.Validate(); err != nil {
		return KnowledgeEntry{}, KnowledgePromotion{}, err
	}
	if err := ValidatePromotionConsistency(promoted, record); err != nil {
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
		// A promoted entry never carries an owner reference, regardless of
		// its resulting scope: visibility for it comes from having been
		// promoted, never from ownership (OwnedKnowledge), even for the edge
		// case of promoting an execution-scoped claim to work_unit scope.
		if e.OwnerRef != "" {
			return errors.New("a promoted knowledge entry may not carry an owner reference")
		}
		// A scope rank alone is not an audience: work_unit and feature scope
		// promotions MUST name who they are for, or they would be readable
		// by every unit/feature in the product; product and organization
		// scope have no narrower audience to restrict to.
		switch e.Scope {
		case ScopeWorkUnit, ScopeFeature:
			if err := boundedField("knowledge entry audience reference", e.AudienceRef, maxKnowledgeAudienceBytes); err != nil {
				return err
			}
		case ScopeProduct, ScopeOrganization:
			if e.AudienceRef != "" {
				return fmt.Errorf("a promotion to %q scope may not carry an audience reference; it has no narrower audience to restrict to", e.Scope)
			}
		}
	default:
		if e.PromotedFrom != nil {
			return fmt.Errorf("knowledge entry provenance %q may not carry a promoted-from source", e.Provenance.Type)
		}
		if e.AudienceRef != "" {
			return errors.New("a directly authored knowledge entry may not carry an audience reference; only a promotion names one")
		}
		switch e.Scope {
		case ScopeExecution, ScopeWorkUnit:
			if err := boundedField("knowledge entry owner reference", e.OwnerRef, maxKnowledgeOwnerRefBytes); err != nil {
				return err
			}
		default:
			if e.OwnerRef != "" {
				return fmt.Errorf("a %q-scoped knowledge entry may not carry an owner reference", e.Scope)
			}
		}
	}
	id, err := e.contentID()
	if err != nil {
		return err
	}
	if id != e.ID {
		return fmt.Errorf("knowledge entry id %s does not match its own recomputed content id %s", e.ID, id)
	}
	return nil
}

func (e KnowledgeEntry) contentID() (string, error) {
	digest, err := domain.Digest(struct {
		ProductID    string     `json:"product_id"`
		Scope        Scope      `json:"scope"`
		OwnerRef     string     `json:"owner_ref,omitempty"`
		AudienceRef  string     `json:"audience_ref,omitempty"`
		Statement    string     `json:"statement"`
		Provenance   Provenance `json:"provenance"`
		PromotedFrom *string    `json:"promoted_from,omitempty"`
		CreatedAt    string     `json:"created_at"`
	}{e.ProductID, e.Scope, e.OwnerRef, e.AudienceRef, e.Statement, e.Provenance, e.PromotedFrom, e.CreatedAt.UTC().Format(time.RFC3339Nano)})
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
	// AudienceRef mirrors the promoted entry's own AudienceRef, so the audit
	// record states the intended audience as part of what was authorized,
	// not only as a field on the entry it produced.
	AudienceRef     string    `json:"audience_ref,omitempty"`
	AuthorizedBy    string    `json:"authorized_by"`
	PromotedEntryID string    `json:"promoted_entry_id"`
	PromotedAt      time.Time `json:"promoted_at"`
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
	switch r.TargetScope {
	case ScopeWorkUnit, ScopeFeature:
		if err := boundedField("promotion audience reference", r.AudienceRef, maxKnowledgeAudienceBytes); err != nil {
			return err
		}
	case ScopeProduct, ScopeOrganization:
		if r.AudienceRef != "" {
			return fmt.Errorf("a promotion to %q scope may not carry an audience reference", r.TargetScope)
		}
	}
	if r.PromotedAt.IsZero() {
		return errors.New("promotion time is required")
	}
	id, err := r.contentID()
	if err != nil {
		return err
	}
	if id != r.ID {
		return fmt.Errorf("promotion id %s does not match its own recomputed content id %s", r.ID, id)
	}
	return nil
}

func (r KnowledgePromotion) contentID() (string, error) {
	digest, err := domain.Digest(struct {
		SourceEntryID   string `json:"source_entry_id"`
		TargetScope     Scope  `json:"target_scope"`
		AudienceRef     string `json:"audience_ref,omitempty"`
		AuthorizedBy    string `json:"authorized_by"`
		PromotedAt      string `json:"promoted_at"`
		PromotedEntryID string `json:"promoted_entry_id"`
	}{r.SourceEntryID, r.TargetScope, r.AudienceRef, r.AuthorizedBy, r.PromotedAt.UTC().Format(time.RFC3339Nano), r.PromotedEntryID})
	if err != nil {
		return "", err
	}
	return "promotion-" + digest[:32], nil
}

// ValidatePromotionConsistency checks that a promoted entry and its audit
// record describe the SAME action - not merely that each is independently
// self-consistent. Two documents can each pass Validate on their own and
// still disagree about what happened: a record whose target scope,
// authorizer or timestamp does not match the promoted entry it claims to
// audit is exactly that, and is refused here rather than only checked by id.
// Callers call this AFTER promoted.Validate() and record.Validate(), which
// already establish that promoted.Provenance.Type is ProvenancePromotion
// whenever PromotedFrom is set.
func ValidatePromotionConsistency(promoted KnowledgeEntry, record KnowledgePromotion) error {
	if record.PromotedEntryID != promoted.ID {
		return fmt.Errorf("promotion record names entry %s but was given entry %s", record.PromotedEntryID, promoted.ID)
	}
	if promoted.PromotedFrom == nil || *promoted.PromotedFrom != record.SourceEntryID {
		return errors.New("promoted entry does not link back to the promotion record's source entry")
	}
	if promoted.Scope != record.TargetScope {
		return fmt.Errorf("promoted entry scope %q does not match the promotion record's target scope %q", promoted.Scope, record.TargetScope)
	}
	if promoted.AudienceRef != record.AudienceRef {
		return errors.New("promoted entry audience reference does not match the promotion record")
	}
	if promoted.Provenance.Producer != record.AuthorizedBy {
		return errors.New("promoted entry's producer does not match the promotion record's authorizer")
	}
	if !promoted.CreatedAt.Equal(record.PromotedAt) {
		return errors.New("promoted entry's creation time does not match the promotion record's promoted-at time")
	}
	return nil
}
