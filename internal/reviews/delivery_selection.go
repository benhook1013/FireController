package reviews

// Review identity/history is independent of deliberate Delivery enrollment.
// Existing catalog entries remain selected; first-use worker reports create an
// unselected identity. Selection travels with immutable PR versions.
func deliverySelectionMigrationSQL() []string {
	return []string{
		`ALTER TABLE review_pull_versions ADD COLUMN delivery_selected INTEGER NOT NULL DEFAULT 1 CHECK (delivery_selected IN (0,1))`,
		`DROP VIEW review_pulls`, HistoryReadViewSQL()["review_pulls"],
	}
}
