package reviewexec

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
		{"e4", "p", "cli", `{"capture":{"review":{"Findings":[{"Title":"Valid finding"}]}}}`, 2},
		{"e1", "p", "cli", `{"capture":{"review":{"Findings":[]}}}`, 2},
		{"e3", "p", "hosted", `{"capture":{"review":{"Findings":null}}}`, 2},
		{"e2", "p", "hosted", `{"capture":{"review":{"Findings":[{"Title":"Valid finding"}]}}}`, 2},
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
	// Freeze the original query label, NUL separator, and ordered JSON rows.
	// CLI includes e1, e2, e4; Hosted includes e2, e3, e4.
	for _, tc := range []struct{ channel, digest string }{
		{"cli", "4dc8d8a0206132b93a7fc669f6bfa0046ce6e1891faf238a0626313e21415d99"},
		{"hosted", "1b1e87c3e6f25b7e538a6e0197be5baf4d1a945228126d45ef4d2141664a5eb2"},
	} {
		var actual bytes.Buffer
		label := automaticAdjudicationBasisLabel(tc.channel)
		if err := automaticBasisAppender(t.Context(), tx, &actual, tc.channel, 1)(label, false, "p", 2); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(actual.Bytes())
		if got := hex.EncodeToString(sum[:]); got != tc.digest {
			t.Fatalf("%s changed valid digest input: got %s, want %s; input %q", tc.channel, got, tc.digest, actual.Bytes())
		}
	}
}
