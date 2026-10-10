package runtime

// Product and ProductConfiguration persistence (#476).
//
// Both AdoptProductRevision and AdoptProductConfigurationRevision run inside
// one BEGIN IMMEDIATE transaction (sqlite_store.go's _txlock=immediate: the
// write lock is held from the transaction's first statement, so no concurrent
// adopter - in this process or another - can read a stale "current" between
// this one's check and its commit). Each enforces the FULL revision-transition
// invariant, not merely a valid-looking document: the first revision for a
// product or configuration must be 1, a later one must be EXACTLY the
// successor of whatever is current, and product.ValidateProductMutation /
// product.ValidateConfigurationMutation run against that exact current
// revision before anything is written. Resubmitting an already-adopted exact
// revision is still idempotent, checked by digest before the mutation path is
// ever reached.

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/product"
)

// SQLiteOperationStore satisfies product.Store: CompileContext can take it
// directly, with no adapter in between.
var _ product.Store = (*SQLiteOperationStore)(nil)

// AdoptProductRevision admits one revision under the full transition
// invariant described above.
func (s *SQLiteOperationStore) AdoptProductRevision(p product.Product) (product.Product, bool, error) {
	if err := p.Validate(); err != nil {
		return product.Product{}, false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return product.Product{}, false, err
	}
	defer tx.Rollback()

	existing, found, err := productRevisionTx(tx, p.ID, p.Revision)
	if err != nil {
		return product.Product{}, false, err
	}
	if found {
		storedDigest, err := existing.RevisionDigest()
		if err != nil {
			return product.Product{}, false, err
		}
		proposedDigest, err := p.RevisionDigest()
		if err != nil {
			return product.Product{}, false, err
		}
		if storedDigest != proposedDigest {
			return product.Product{}, false, fmt.Errorf(
				"product %s revision %d is already adopted with different contents; a revision is immutable, so propose a later revision instead",
				p.ID, p.Revision)
		}
		return existing, false, tx.Commit()
	}

	current, hasCurrent, err := currentProductTx(tx, p.ID)
	if err != nil {
		return product.Product{}, false, err
	}
	switch {
	case !hasCurrent:
		if p.Revision != 1 {
			return product.Product{}, false, fmt.Errorf(
				"product %s has no revisions yet; its first revision must be 1, not %d", p.ID, p.Revision)
		}
	case p.Revision != current.Revision+1:
		return product.Product{}, false, fmt.Errorf(
			"product %s is at revision %d; the next revision is %d, not %d",
			p.ID, current.Revision, current.Revision+1, p.Revision)
	default:
		if err := product.ValidateProductMutation(current, p); err != nil {
			return product.Product{}, false, err
		}
	}

	document, err := CanonicalJSON(p)
	if err != nil {
		return product.Product{}, false, err
	}
	if _, err := tx.Exec(`INSERT INTO product_revisions (product_id, revision, name, created_unix_nano, document)
		VALUES (?, ?, ?, ?, ?)`, p.ID, p.Revision, p.Name, p.CreatedAt.UnixNano(), string(document)); err != nil {
		return product.Product{}, false, err
	}
	return p, true, tx.Commit()
}

// ProductRevision reads one exact revision.
func (s *SQLiteOperationStore) ProductRevision(productID string, revision int) (product.Product, bool, error) {
	return productRevisionTx(s.db, productID, revision)
}

// CurrentProduct reads a product's current revision: the highest one adopted.
// It satisfies product.Store.
func (s *SQLiteOperationStore) CurrentProduct(productID string) (product.Product, bool, error) {
	return currentProductTx(s.db, productID)
}

// Products reads one bounded page of every product's current revision,
// oldest product first: a database-level LIMIT/OFFSET, never a full scan
// trimmed in Go. hasMore is true when a (limit+1)th row exists beyond the
// page returned, the same cursor contract control-plane's event pages use.
func (s *SQLiteOperationStore) Products(offset, limit int) ([]product.Product, bool, error) {
	rows, err := s.db.Query(`SELECT document FROM product_revisions AS current
		WHERE revision = (SELECT MAX(revision) FROM product_revisions WHERE product_id = current.product_id)
		ORDER BY created_unix_nano ASC, product_id ASC LIMIT ? OFFSET ?`, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var products []product.Product
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, false, err
		}
		p, err := decodeProduct(document)
		if err != nil {
			return nil, false, err
		}
		products = append(products, p)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(products) > limit
	if hasMore {
		products = products[:limit]
	}
	return products, hasMore, nil
}

func productRevisionTx(db sqlExecutor, productID string, revision int) (product.Product, bool, error) {
	var document string
	err := db.QueryRow(`SELECT document FROM product_revisions WHERE product_id = ? AND revision = ?`,
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

func currentProductTx(db sqlExecutor, productID string) (product.Product, bool, error) {
	var document string
	err := db.QueryRow(`SELECT document FROM product_revisions WHERE product_id = ?
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

// AdoptProductConfigurationRevision admits one configuration revision under
// the same full transition invariant AdoptProductRevision enforces, plus one
// more precondition: the product it configures must already exist.
func (s *SQLiteOperationStore) AdoptProductConfigurationRevision(c product.ProductConfiguration) (product.ProductConfiguration, bool, error) {
	if err := c.Validate(); err != nil {
		return product.ProductConfiguration{}, false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return product.ProductConfiguration{}, false, err
	}
	defer tx.Rollback()

	if _, found, err := currentProductTx(tx, c.ProductID); err != nil {
		return product.ProductConfiguration{}, false, err
	} else if !found {
		return product.ProductConfiguration{}, false, fmt.Errorf("product %s does not exist; adopt a product revision first", c.ProductID)
	}

	existing, found, err := configurationRevisionTx(tx, c.ProductID, c.Revision)
	if err != nil {
		return product.ProductConfiguration{}, false, err
	}
	if found {
		storedDigest, err := existing.RevisionDigest()
		if err != nil {
			return product.ProductConfiguration{}, false, err
		}
		proposedDigest, err := c.RevisionDigest()
		if err != nil {
			return product.ProductConfiguration{}, false, err
		}
		if storedDigest != proposedDigest {
			return product.ProductConfiguration{}, false, fmt.Errorf(
				"product %s configuration revision %d is already adopted with different contents; propose a later revision instead",
				c.ProductID, c.Revision)
		}
		return existing, false, tx.Commit()
	}

	current, hasCurrent, err := currentConfigurationTx(tx, c.ProductID)
	if err != nil {
		return product.ProductConfiguration{}, false, err
	}
	switch {
	case !hasCurrent:
		if c.Revision != 1 {
			return product.ProductConfiguration{}, false, fmt.Errorf(
				"product %s has no configuration yet; its first revision must be 1, not %d", c.ProductID, c.Revision)
		}
	case c.Revision != current.Revision+1:
		return product.ProductConfiguration{}, false, fmt.Errorf(
			"product %s configuration is at revision %d; the next revision is %d, not %d",
			c.ProductID, current.Revision, current.Revision+1, c.Revision)
	default:
		if err := product.ValidateConfigurationMutation(current, c); err != nil {
			return product.ProductConfiguration{}, false, err
		}
	}

	document, err := CanonicalJSON(c)
	if err != nil {
		return product.ProductConfiguration{}, false, err
	}
	if _, err := tx.Exec(`INSERT INTO product_configuration_revisions (product_id, revision, created_unix_nano, document)
		VALUES (?, ?, ?, ?)`, c.ProductID, c.Revision, c.CreatedAt.UnixNano(), string(document)); err != nil {
		return product.ProductConfiguration{}, false, err
	}
	return c, true, tx.Commit()
}

// ConfigurationRevision reads one exact configuration revision. It satisfies
// product.Store, so a caller pinning a WorkUnit's frozen configuration gets
// exactly that revision rather than whatever is current.
func (s *SQLiteOperationStore) ConfigurationRevision(productID string, revision int) (product.ProductConfiguration, bool, error) {
	return configurationRevisionTx(s.db, productID, revision)
}

// CurrentConfiguration reads a product's current configuration revision: the
// highest one adopted. It satisfies product.Store. found is false for a
// product with no configuration adopted yet, which is not an error.
func (s *SQLiteOperationStore) CurrentConfiguration(productID string) (product.ProductConfiguration, bool, error) {
	return currentConfigurationTx(s.db, productID)
}

func configurationRevisionTx(db sqlExecutor, productID string, revision int) (product.ProductConfiguration, bool, error) {
	var document string
	err := db.QueryRow(`SELECT document FROM product_configuration_revisions WHERE product_id = ? AND revision = ?`,
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

func currentConfigurationTx(db sqlExecutor, productID string) (product.ProductConfiguration, bool, error) {
	var document string
	err := db.QueryRow(`SELECT document FROM product_configuration_revisions WHERE product_id = ?
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
