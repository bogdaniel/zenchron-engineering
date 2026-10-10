package runtime

// WorkGraph-to-product association persistence (#476).
//
// AssociateWorkGraph verifies BOTH sides exist - inside the same BEGIN
// IMMEDIATE transaction as the insert - before claiming anything: a graph id
// that was never adopted, or a product id that was never adopted, is refused
// rather than stored as a disconnected identity nothing else can ever resolve.

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
	"github.com/bogdaniel/zenchron-engineering/product"
)

// AssociateWorkGraph claims one graph for one product. It is insert-only, and
// the primary key is graph_id alone: a graph belongs to at most one product,
// so a second product can never silently start reading its units' context.
// The SAME association replayed is idempotent; a DIFFERENT product claiming
// an already-associated graph is refused.
func (s *SQLiteOperationStore) AssociateWorkGraph(association product.GraphAssociation) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	if _, found, err := currentProductTx(tx, association.ProductID); err != nil {
		return false, err
	} else if !found {
		return false, fmt.Errorf("product %s does not exist", association.ProductID)
	}
	if _, found, err := currentWorkGraphTx(tx, association.GraphID); err != nil {
		return false, err
	} else if !found {
		return false, fmt.Errorf("work graph %s does not exist", association.GraphID)
	}

	result, err := tx.Exec(`INSERT INTO product_graph_associations (graph_id, product_id, associated_unix_nano)
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
		return true, tx.Commit()
	}
	var existing string
	if err := tx.QueryRow(`SELECT product_id FROM product_graph_associations WHERE graph_id = ?`,
		association.GraphID).Scan(&existing); err != nil {
		return false, err
	}
	if existing != association.ProductID {
		return false, fmt.Errorf("work graph %s is already associated with product %s, not %s",
			association.GraphID, existing, association.ProductID)
	}
	return false, tx.Commit()
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

// AssociatedGraphs reads one bounded page of graph ids associated with a
// product, the reverse of AssociatedProduct: a database-level LIMIT/OFFSET,
// never a silently truncated full read. hasMore is true when a (limit+1)th
// row exists beyond the page returned, so a caller can show it (or page
// into it) rather than present a 201st association as if it did not exist.
func (s *SQLiteOperationStore) AssociatedGraphs(productID string, offset, limit int) ([]string, bool, error) {
	rows, err := s.db.Query(`SELECT graph_id FROM product_graph_associations
		WHERE product_id = ? ORDER BY associated_unix_nano ASC, graph_id ASC LIMIT ? OFFSET ?`,
		productID, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var graphIDs []string
	for rows.Next() {
		var graphID string
		if err := rows.Scan(&graphID); err != nil {
			return nil, false, err
		}
		graphIDs = append(graphIDs, graphID)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(graphIDs) > limit
	if hasMore {
		graphIDs = graphIDs[:limit]
	}
	return graphIDs, hasMore, nil
}

// currentWorkGraphTx mirrors runtime.WorkGraph (workgraph_store.go), but takes
// a sqlExecutor so AssociateWorkGraph can read it inside its own transaction
// instead of racing a second, separate read against the same table.
func currentWorkGraphTx(db sqlExecutor, graphID string) (orchestration.WorkGraph, bool, error) {
	var document string
	err := db.QueryRow(`SELECT document FROM work_graph_revisions WHERE graph_id = ?
		ORDER BY revision DESC LIMIT 1`, graphID).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.WorkGraph{}, false, nil
	}
	if err != nil {
		return orchestration.WorkGraph{}, false, err
	}
	graph, err := decodeWorkGraph(document)
	return graph, err == nil, err
}
