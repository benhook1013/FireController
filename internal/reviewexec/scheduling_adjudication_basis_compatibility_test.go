package reviewexec

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"testing"
)

func TestAutomaticAdjudicationBasisPreservesValidRetainedDigest(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, ddl := range []string{
		"CREATE TABLE review_adjudication_state(evidence_id TEXT,revision INTEGER,project_id TEXT)",
		"CREATE TABLE review_execution_evidence(id TEXT,project_id TEXT,operation_id TEXT,body_json TEXT)",
		"CREATE TABLE review_operations(id TEXT,project_id TEXT,pull_number INTEGER,channel TEXT)",
	} {
		if _, err = tx.ExecContext(t.Context(), ddl); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		id, project, channel, body string
		pull                       int
	}{
		{"e1", "p", "cli", `{"capture":{"review":{"Findings":[]}}}`, 2},
		{"e2", "p", "hosted", `{"capture":{"review":{"Findings":[{"Title":"Valid finding"}]}}}`, 2},
		{"e3", "p", "hosted", `{"capture":{"review":{"Findings":null}}}`, 2},
		{"e4", "p", "cli", `{"capture":{"review":{"Findings":[{"Title":"Valid finding"}]}}}`, 2},
		{"e5", "p", "hosted", `{"capture":{"review":{"Findings":[]}}}`, 4},
		{"e6", "q", "cli", `{"capture":{"review":{"Findings":[{"Title":"Valid finding"}]}}}`, 2},
	} {
		if _, err = tx.ExecContext(t.Context(), "INSERT INTO review_adjudication_state VALUES(?,1,?)", row.id, row.project); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(t.Context(), "INSERT INTO review_execution_evidence VALUES(?,?,?,?)", row.id, row.project, row.id, row.body); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(t.Context(), "INSERT INTO review_operations VALUES(?,?,?,?)", row.id, row.project, row.pull, row.channel); err != nil {
			t.Fatal(err)
		}
	}
	for _, channel := range []string{"cli", "hosted"} {
		label := automaticAdjudicationBasisLabel(channel)
		var before, after bytes.Buffer
		before.WriteString(label)
		before.WriteByte(0)
		rows, err := tx.QueryContext(t.Context(), label, "p", 2)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id string
			var revision int64
			if err = rows.Scan(&id, &revision); err != nil {
				t.Fatal(err)
			}
			if err = json.NewEncoder(&before).Encode([]any{id, revision}); err != nil {
				t.Fatal(err)
			}
		}
		if err = rows.Close(); err != nil {
			t.Fatal(err)
		}
		if err = automaticBasisAppender(t.Context(), tx, &after, channel, 1)(label, false, "p", 2); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before.Bytes(), after.Bytes()) {
			t.Fatalf("%s changed valid digest input", channel)
		}
	}
}
