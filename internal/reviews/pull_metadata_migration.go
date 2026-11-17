package reviews

func pullMetadataMigrationSQL() []string {
	return []string{
		`ALTER TABLE review_pull_versions ADD COLUMN metadata_json TEXT NOT NULL DEFAULT '{}' CHECK (length(CAST(metadata_json AS BLOB)) <= 512)`,
		`DROP VIEW review_pulls`, HistoryReadViewSQLV7()["review_pulls"],
	}
}
