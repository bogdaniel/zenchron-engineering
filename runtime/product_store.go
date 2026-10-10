package runtime

// ProductEngineeringEnvironment persistence (#476). Five tables, all in the
// same database runs, operations and the journal already use - #471 forbids a
// second run/task database, and a product's own state is not one either: it
// is identity, mutable configuration, graph association and knowledge, none
// of which is a run, an operation or an event.
//
// product_revisions and product_configuration_revisions are append-only,
// mirroring work_graph_revisions (runtime/workgraph_store.go): a revision is
// written once and kept, and "current" is always the highest one adopted.
// product_graph_associations and product_knowledge_entries are append-only
// facts with no revision concept of their own. product_knowledge_promotions
// is the durable audit trail of every authorized scope widening.

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/product"
)

// SQLiteOperationStore satisfies product.Store: CompileContext can take it
// directly, with no adapter in between.
var _ product.Store = (*SQLiteOperationStore)(nil)

// AdoptProductRevision writes one revision ONCE, exactly like
// AdoptWorkGraphRevision: a resubmission of an already-adopted revision is
// found rather than rewritten, and a different document under the same
// revision number is a conflict rather than an overwrite.
func (s *SQLiteOperationStore) AdoptProductRevision(p product.Product) (product.Product, bool, error) {
	if err := p.Validate(); err != nil {
		return product.Product{}, false, err
	}
	document, err := CanonicalJSON(p)
	if err != nil {
		return product.Product{}, false, err
	}
	result, err := s.db.Exec(`INSERT INTO product_revisions (product_id, revision, name, created_unix_nano, document)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(product_id, revision) DO NOTHING`,
		p.ID, p.Revision, p.Name, p.CreatedAt.UnixNano(), string(document))
	if err != nil {
		return product.Product{}, false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return product.Product{}, false, err
	}
	if inserted == 1 {
		return p, true, nil
	}
	stored, found, err := s.ProductRevision(p.ID, p.Revision)
	if err != nil {
		return product.Product{}, false, err
	}
	if !found {
		return product.Product{}, false, fmt.Errorf("product %s revision %d was neither inserted nor found", p.ID, p.Revision)
	}
	storedDigest, err := stored.RevisionDigest()
	if err != nil {
		return product.Product{}, false, err
	}
	proposedDigest, err := p.RevisionDigest()
	if err != nil {
		return product.Product{}, false, err
	}
	if storedDigest != proposedDigest {
		return product.Product{}, false, fmt.Errorf(
			"product %s revision %d is already adopted with different contents; a revision is immutable, so propose revision %d instead",
			p.ID, p.Revision, p.Revision+1)
	}
	return stored, false, nil
}

// ProductRevision reads one exact revision.
func (s *SQLiteOperationStore) ProductRevision(productID string, revision int) (product.Product, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM product_revisions WHERE product_id = ? AND revision = ?`,
		productID, revision).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return product.Product{}, false, nil
	}
	if err != nil {
		return product.Product{}, false, err
	}
	p, err := decodeProduct(document)
	return p, err == nil, err
}

// CurrentProduct reads a product's current revision: the highest one adopted.
// It satisfies product.Store.
func (s *SQLiteOperationStore) CurrentProduct(productID string) (product.Product, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM product_revisions WHERE product_id = ?
		ORDER BY revision DESC LIMIT 1`, productID).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return product.Product{}, false, nil
	}
	if err != nil {
		return product.Product{}, false, err
	}
	p, err := decodeProduct(document)
	return p, err == nil, err
}

func decodeProduct(document string) (product.Product, error) {
	var p product.Product
	if err := strictJSON([]byte(document), &p); err != nil {
		return product.Product{}, fmt.Errorf("stored product is unreadable: %w", err)
	}
	if err := p.Validate(); err != nil {
		return product.Product{}, fmt.Errorf("stored product is corrupt: %w", err)
	}
	return p, nil
}

// AdoptProductConfigurationRevision mirrors AdoptProductRevision for the
// mutable half of #476: the frozen history of every past revision is kept
// exactly as the operator adopted it, and only the pointer to "current" moves.
func (s *SQLiteOperationStore) AdoptProductConfigurationRevision(c product.ProductConfiguration) (product.ProductConfiguration, bool, error) {
	if err := c.Validate(); err != nil {
		return product.ProductConfiguration{}, false, err
	}
	document, err := CanonicalJSON(c)
	if err != nil {
		return product.ProductConfiguration{}, false, err
	}
	result, err := s.db.Exec(`INSERT INTO product_configuration_revisions (product_id, revision, created_unix_nano, document)
		VALUES (?, ?, ?, ?) ON CONFLICT(product_id, revision) DO NOTHING`,
		c.ProductID, c.Revision, c.CreatedAt.UnixNano(), string(document))
	if err != nil {
		return product.ProductConfiguration{}, false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return product.ProductConfiguration{}, false, err
	}
	if inserted == 1 {
		return c, true, nil
	}
	stored, found, err := s.ProductConfigurationRevision(c.ProductID, c.Revision)
	if err != nil {
		return product.ProductConfiguration{}, false, err
	}
	if !found {
		return product.ProductConfiguration{}, false, fmt.Errorf(
			"product %s configuration revision %d was neither inserted nor found", c.ProductID, c.Revision)
	}
	storedDigest, err := stored.RevisionDigest()
	if err != nil {
		return product.ProductConfiguration{}, false, err
	}
	proposedDigest, err := c.RevisionDigest()
	if err != nil {
		return product.ProductConfiguration{}, false, err
	}
	if storedDigest != proposedDigest {
		return product.ProductConfiguration{}, false, fmt.Errorf(
			"product %s configuration revision %d is already adopted with different contents; propose revision %d instead",
			c.ProductID, c.Revision, c.Revision+1)
	}
	return stored, false, nil
}

// ProductConfigurationRevision reads one exact configuration revision.
func (s *SQLiteOperationStore) ProductConfigurationRevision(productID string, revision int) (product.ProductConfiguration, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM product_configuration_revisions WHERE product_id = ? AND revision = ?`,
		productID, revision).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return product.ProductConfiguration{}, false, nil
	}
	if err != nil {
		return product.ProductConfiguration{}, false, err
	}
	c, err := decodeProductConfiguration(document)
	return c, err == nil, err
}

// CurrentConfiguration reads a product's current configuration revision: the
// highest one adopted. It satisfies product.Store. found is false for a
// product with no configuration adopted yet, which is not an error.
func (s *SQLiteOperationStore) CurrentConfiguration(productID string) (product.ProductConfiguration, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM product_configuration_revisions WHERE product_id = ?
		ORDER BY revision DESC LIMIT 1`, productID).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return product.ProductConfiguration{}, false, nil
	}
	if err != nil {
		return product.ProductConfiguration{}, false, err
	}
	c, err := decodeProductConfiguration(document)
	return c, err == nil, err
}

func decodeProductConfiguration(document string) (product.ProductConfiguration, error) {
	var c product.ProductConfiguration
	if err := strictJSON([]byte(document), &c); err != nil {
		return product.ProductConfiguration{}, fmt.Errorf("stored product configuration is unreadable: %w", err)
	}
	if err := c.Validate(); err != nil {
		return product.ProductConfiguration{}, fmt.Errorf("stored product configuration is corrupt: %w", err)
	}
	return c, nil
}

// AssociateWorkGraph claims one graph for one product. It is insert-only, and
// the primary key is graph_id alone: a graph belongs to at most one product,
// so a second product can never silently start reading its units' context.
// The SAME association replayed is idempotent; a DIFFERENT product claiming
// an already-associated graph is refused.
func (s *SQLiteOperationStore) AssociateWorkGraph(association product.GraphAssociation) (bool, error) {
	result, err := s.db.Exec(`INSERT INTO product_graph_associations (graph_id, product_id, associated_unix_nano)
		VALUES (?, ?, ?) ON CONFLICT(graph_id) DO NOTHING`,
		association.GraphID, association.ProductID, association.AssociatedAt.UnixNano())
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if inserted == 1 {
		return true, nil
	}
	var existing string
	if err := s.db.QueryRow(`SELECT product_id FROM product_graph_associations WHERE graph_id = ?`,
		association.GraphID).Scan(&existing); err != nil {
		return false, err
	}
	if existing != association.ProductID {
		return false, fmt.Errorf("work graph %s is already associated with product %s, not %s",
			association.GraphID, existing, association.ProductID)
	}
	return false, nil
}

// AssociatedProduct reads which product, if any, a graph is associated with.
func (s *SQLiteOperationStore) AssociatedProduct(graphID string) (string, bool, error) {
	var productID string
	err := s.db.QueryRow(`SELECT product_id FROM product_graph_associations WHERE graph_id = ?`, graphID).Scan(&productID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return productID, err == nil, err
}

// RecordKnowledgeEntry writes one directly authored entry. It is insert-only
// and idempotent by the entry's own content identity.
func (s *SQLiteOperationStore) RecordKnowledgeEntry(entry product.KnowledgeEntry) (bool, error) {
	if err := entry.Validate(); err != nil {
		return false, err
	}
	return s.insertKnowledgeEntry(s.db, entry)
}

// RecordPromotion writes a promoted entry and its audit record atomically: a
// promotion is never half-durable. Replaying the same Promote output is
// idempotent, since both ids are content-derived.
func (s *SQLiteOperationStore) RecordPromotion(promoted product.KnowledgeEntry, record product.KnowledgePromotion) (bool, error) {
	if err := promoted.Validate(); err != nil {
		return false, err
	}
	if err := record.Validate(); err != nil {
		return false, err
	}
	if record.PromotedEntryID != promoted.ID {
		return false, fmt.Errorf("promotion record names entry %s but was given entry %s", record.PromotedEntryID, promoted.ID)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	entryCreated, err := s.insertKnowledgeEntry(tx, promoted)
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
	return entryCreated || recordCreated == 1, tx.Commit()
}

func (s *SQLiteOperationStore) insertKnowledgeEntry(db sqlExecutor, entry product.KnowledgeEntry) (bool, error) {
	rank, ok := product.ScopeRank(entry.Scope)
	if !ok {
		return false, fmt.Errorf("knowledge entry scope %q is not a known scope", entry.Scope)
	}
	document, err := CanonicalJSON(entry)
	if err != nil {
		return false, err
	}
	result, err := db.Exec(`INSERT INTO product_knowledge_entries
		(id, product_id, scope, scope_rank, created_unix_nano, document)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		entry.ID, entry.ProductID, string(entry.Scope), rank, entry.CreatedAt.UnixNano(), string(document))
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	return inserted == 1, err
}

// KnowledgeAtOrBelow returns one product's knowledge entries whose scope is at
// or below the given boundary, oldest first. It satisfies product.Store, and
// the product_id predicate is the whole cross-product isolation guarantee:
// this query can never return a row belonging to another product.
func (s *SQLiteOperationStore) KnowledgeAtOrBelow(productID string, scope product.Scope) ([]product.KnowledgeEntry, error) {
	rank, ok := product.ScopeRank(scope)
	if !ok {
		return nil, fmt.Errorf("scope %q is not a known scope", scope)
	}
	rows, err := s.db.Query(`SELECT document FROM product_knowledge_entries
		WHERE product_id = ? AND scope_rank <= ? ORDER BY created_unix_nano ASC, id ASC`, productID, rank)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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
