package reviewexec

import "database/sql"

// snapshotQueries reuses a bounded set of SQL programs within this validator's
// existing transaction. It never retains row/results or changes queries, order,
// context, or evidence validation. Other readers preserve their original path.
func snapshotQueries(q EvidenceQuery) (EvidenceQuery, func()) {
	if tx, ok := q.(*sql.Tx); ok {
		prepared := newOccupancyQueries(tx)
		return prepared, prepared.close
	}
	return q, func() {}
}
