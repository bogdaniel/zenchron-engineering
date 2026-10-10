package runtime

// Knowledge entry and promotion persistence (#476).
//
// Visibility is split into three queries - OwnedKnowledge,
// WidelyPromotedKnowledgeAtOrBelow, AudiencedPromotedKnowledge - never one
// "scope_rank <= requested" read: the provenance_type and audience_ref
// columns are indexed alongside scope_rank specifically so a wide-scope
// reader's query can never return a raw, unpromoted discovery or an
// audience-restricted promotion meant for someone else, and an owner-scoped
// reader's query can never return another owner's discovery or a promoted
// entry (which has no owner). All three are bounded by
// product.MaxContextKnowledgeEntries so a product with many entries cannot
// hand one caller an unbounded context.
//
// RecordKnowledgeEntry refuses promotion-provenance outright: a promoted
// entry's audit trail is written ONLY by RecordPromotion, atomically with the
// entry itself, inside one transaction that also re-verifies the claimed
// source entry against what is actually stored - its product, scope,
// provenance and statement - rather than trusting the caller's documents, and
// cross-checks the promoted entry against its own audit record (not merely
// that each independently validates) before writing either. A conflicting
// replay - same promotion id, different content - is refused rather than
// silently ignored, which would otherwise leave an orphan promoted entry with
// no matching audit row.

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/product"
)

// RecordKnowledgeEntry writes one directly authored entry. It is insert-only
// and idempotent by the entry's own content identity.
func (s *SQLiteOperationStore) RecordKnowledgeEntry(entry product.KnowledgeEntry) (bool, error) {
	if entry.Provenance.Type == product.ProvenancePromotion {
		return false, errors.New(
			"a promoted knowledge entry must be written through RecordPromotion, which also writes its audit record")
	}
	if err := entry.Validate(); err != nil {
		return false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, found, err := currentProductTx(tx, entry.ProductID); err != nil {
		return false, err
	} else if !found {
		return false, fmt.Errorf("product %s does not exist", entry.ProductID)
	}
	created, err := insertKnowledgeEntry(tx, entry)
	if err != nil {
		return false, err
	}
	return created, tx.Commit()
}

// RecordPromotion writes a promoted entry and its audit record atomically: a
// promotion is never half-durable. Before writing anything, it cross-checks
// the promoted entry against its own audit record - not merely that each
// independently validates, which two mutually contradictory documents can
// both do - and re-derives the claimed source entry from storage, refusing a
// promotion whose source does not exist, belongs to a different product, or
// disagrees with the audit record about scope, provenance or content.
//
// Replaying the EXACT same Promote output is idempotent, since both ids are
// content-derived and the promotion id now covers PromotedEntryID too - two
// different promoted entries can never share one promotion id by construction.
// As a second, transactional line of defense, a conflicting replay - the same
// promotion id with a stored audit document that disagrees with the one being
// written - is refused rather than silently ignored: a blind
// "ON CONFLICT DO NOTHING" there would let a new, unaudited promoted entry
// commit while the audit insert it depended on was dropped underneath it.
func (s *SQLiteOperationStore) RecordPromotion(promoted product.KnowledgeEntry, record product.KnowledgePromotion) (bool, error) {
	if err := promoted.Validate(); err != nil {
		return false, err
	}
	if err := record.Validate(); err != nil {
		return false, err
	}
	if err := product.ValidatePromotionConsistency(promoted, record); err != nil {
		return false, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	source, found, err := knowledgeEntryByIDTx(tx, record.SourceEntryID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, fmt.Errorf("promotion source entry %s does not exist", record.SourceEntryID)
	}
	if source.ProductID != promoted.ProductID {
		return false, fmt.Errorf("promotion source entry belongs to product %s, not %s", source.ProductID, promoted.ProductID)
	}
	if source.Scope != record.SourceScope {
		return false, fmt.Errorf("promotion source entry is at scope %q, not the claimed %q", source.Scope, record.SourceScope)
	}
	if source.Provenance != record.SourceProvenance {
		return false, errors.New("promotion source entry provenance does not match the claimed source provenance")
	}
	if source.Statement != promoted.Statement {
		return false, errors.New("promoted entry statement does not match its source entry's statement")
	}

	entryCreated, err := insertKnowledgeEntry(tx, promoted)
	if err != nil {
		return false, err
	}
	document, err := CanonicalJSON(record)
	if err != nil {
		return false, err
	}
	result, err := tx.Exec(`INSERT INTO product_knowledge_promotions
		(id, source_entry_id, promoted_entry_id, promoted_unix_nano, document)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		record.ID, record.SourceEntryID, record.PromotedEntryID, record.PromotedAt.UnixNano(), string(document))
	if err != nil {
		return false, err
	}
	recordCreated, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if recordCreated == 0 {
		var stored string
		if err := tx.QueryRow(`SELECT document FROM product_knowledge_promotions WHERE id = ?`, record.ID).Scan(&stored); err != nil {
			return false, err
		}
		if stored != string(document) {
			return false, fmt.Errorf(
				"promotion %s is already recorded with a different audit document; a promotion record is immutable", record.ID)
		}
	}
	return entryCreated || recordCreated == 1, tx.Commit()
}

func insertKnowledgeEntry(db sqlExecutor, entry product.KnowledgeEntry) (bool, error) {
	rank, ok := product.ScopeRank(entry.Scope)
	if !ok {
		return false, fmt.Errorf("knowledge entry scope %q is not a known scope", entry.Scope)
	}
	document, err := CanonicalJSON(entry)
	if err != nil {
		return false, err
	}
	result, err := db.Exec(`INSERT INTO product_knowledge_entries
		(id, product_id, scope, scope_rank, provenance_type, owner_ref, audience_ref, created_unix_nano, document)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		entry.ID, entry.ProductID, string(entry.Scope), rank, string(entry.Provenance.Type), entry.OwnerRef,
		entry.AudienceRef, entry.CreatedAt.UnixNano(), string(document))
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	return inserted == 1, err
}

func knowledgeEntryByIDTx(db sqlExecutor, id string) (product.KnowledgeEntry, bool, error) {
	var document string
	err := db.QueryRow(`SELECT document FROM product_knowledge_entries WHERE id = ?`, id).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return product.KnowledgeEntry{}, false, nil
	}
	if err != nil {
		return product.KnowledgeEntry{}, false, err
	}
	entry, err := decodeKnowledgeEntry(document)
	return entry, err == nil, err
}

// OwnedKnowledge returns ONLY directly authored entries at or below scope
// that belong to exactly ownerRef, in this product. It satisfies product.Store.
func (s *SQLiteOperationStore) OwnedKnowledge(productID string, scope product.Scope, ownerRef string) ([]product.KnowledgeEntry, error) {
	rank, ok := product.ScopeRank(scope)
	if !ok {
		return nil, fmt.Errorf("scope %q is not a known scope", scope)
	}
	rows, err := s.db.Query(`SELECT document FROM product_knowledge_entries
		WHERE product_id = ? AND owner_ref = ? AND provenance_type != ? AND scope_rank <= ?
		ORDER BY created_unix_nano ASC, id ASC LIMIT ?`,
		productID, ownerRef, string(product.ProvenancePromotion), rank, product.MaxContextKnowledgeEntries)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return decodeKnowledgeRows(rows)
}

// WidelyPromotedKnowledgeAtOrBelow returns promoted entries with NO audience
// restriction (product or organization scope), at or below scope, in this
// product. It satisfies product.Store.
func (s *SQLiteOperationStore) WidelyPromotedKnowledgeAtOrBelow(productID string, scope product.Scope) ([]product.KnowledgeEntry, error) {
	rank, ok := product.ScopeRank(scope)
	if !ok {
		return nil, fmt.Errorf("scope %q is not a known scope", scope)
	}
	rows, err := s.db.Query(`SELECT document FROM product_knowledge_entries
		WHERE product_id = ? AND provenance_type = ? AND audience_ref = '' AND scope_rank <= ?
		ORDER BY created_unix_nano ASC, id ASC LIMIT ?`,
		productID, string(product.ProvenancePromotion), rank, product.MaxContextKnowledgeEntries)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return decodeKnowledgeRows(rows)
}

// AudiencedPromotedKnowledge returns promoted entries targeted at exactly
// scope (work_unit or feature) AND exactly audienceRef, in this product. It
// satisfies product.Store. Matching scope EXACTLY, rather than at or below,
// is deliberate: an audience-restricted promotion names one specific
// work unit or feature, never a range.
func (s *SQLiteOperationStore) AudiencedPromotedKnowledge(productID string, scope product.Scope, audienceRef string) ([]product.KnowledgeEntry, error) {
	if !product.KnownScope(scope) {
		return nil, fmt.Errorf("scope %q is not a known scope", scope)
	}
	rows, err := s.db.Query(`SELECT document FROM product_knowledge_entries
		WHERE product_id = ? AND provenance_type = ? AND scope = ? AND audience_ref = ?
		ORDER BY created_unix_nano ASC, id ASC LIMIT ?`,
		productID, string(product.ProvenancePromotion), string(scope), audienceRef, product.MaxContextKnowledgeEntries)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return decodeKnowledgeRows(rows)
}

func decodeKnowledgeRows(rows *sql.Rows) ([]product.KnowledgeEntry, error) {
	var entries []product.KnowledgeEntry
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, err
		}
		entry, err := decodeKnowledgeEntry(document)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func decodeKnowledgeEntry(document string) (product.KnowledgeEntry, error) {
	var entry product.KnowledgeEntry
	if err := strictJSON([]byte(document), &entry); err != nil {
		return product.KnowledgeEntry{}, fmt.Errorf("stored knowledge entry is unreadable: %w", err)
	}
	if err := entry.Validate(); err != nil {
		return product.KnowledgeEntry{}, fmt.Errorf("stored knowledge entry is corrupt: %w", err)
	}
	return entry, nil
}
