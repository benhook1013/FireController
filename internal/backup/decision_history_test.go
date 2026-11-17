package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"path/filepath"
	"testing"
	"time"
)

func TestDecisionHistoryExportInspectRestoreAndCorruption(t *testing.T) {
	f := newRestoreFixture(t)
	if _, err := reviewexec.New(f.store, nil); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(reviewexec.SelectionDecision{Phase: "automatic_plan", Outcome: "no_new_intent", Reason: "lane_busy", Candidates: []reviewexec.SelectionCandidateDecision{}})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	at := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = f.store.DB().Exec(`INSERT INTO review_selection_decision_history(project_id,channel,phase,fingerprint,body,first_seen_at,last_seen_at,occurrences) VALUES(?,'cli','automatic_plan',?,?,?,?,1)`, f.project, hex.EncodeToString(sum[:]), string(body), at, at); err != nil {
		t.Fatal(err)
	}
	bundle := restoreBundle(t, f)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "restored-history")
	if _, err := Restore(t.Context(), bundle.Path, dest); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(dest, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err = db.QueryRow("SELECT count(*) FROM review_selection_decision_history").Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	for _, mutation := range []string{
		`UPDATE review_selection_decision_history SET fingerprint='bad'`,
		`UPDATE review_selection_decision_history SET body='{}'`,
		`UPDATE review_selection_decision_history SET last_seen_at='invalid'`,
		`DROP INDEX review_selection_decision_lane`,
	} {
		bad := restoreModifiedBundle(t, bundle, mutation)
		if _, err := Inspect(t.Context(), bad); err == nil {
			t.Fatal("accepted corrupt history", mutation)
		}
	}
}
