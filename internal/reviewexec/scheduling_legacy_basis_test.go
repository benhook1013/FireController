package reviewexec

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestAutomaticLegacyBasisKeepsOnlyMatchingCancelledChild(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, statement := range []string{
		"CREATE TABLE review_automatic_selections(project_id,selection_id,pull_number,pull_basis_sha256)",
		"CREATE TABLE review_selections(project_id,id,channel,operation_id)",
		"CREATE TABLE review_operations(project_id,id,pull_number,channel,state)",
		"INSERT INTO review_automatic_selections VALUES ('project','old',7,'legacy')",
		"INSERT INTO review_selections VALUES ('project','old','hosted','child')",
		"INSERT INTO review_operations VALUES ('project','child',7,'hosted','cancelled')",
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, ignore, channel, current, legacy string
		want                                   bool
	}{
		{"exact legacy cancellation", "", "hosted", "current", "legacy", true},
		{"different channel", "", "cli", "current", "legacy", false},
		{"changed source or grant", "", "hosted", "current", "changed", false},
		{"excluded selection", "old", "hosted", "current", "legacy", false},
		{"existing current format comparison", "", "hosted", "legacy", "changed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := automaticPullPreviouslySelected(ctx, tx, "project", tc.ignore, 7, tc.channel, tc.current, tc.legacy)
			if err != nil || got != tc.want {
				t.Fatalf("matched legacy cancellation = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}
