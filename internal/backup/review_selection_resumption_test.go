package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
)

func selectionRestoredActive(t *testing.T, state string) (*activationFixture, ProviderResumptionRequest, string) {
	t.Helper()
	source, id := selectionInventoryFixture(t, state)
	bundle := restoreBundle(t, source)
	dir := filepath.Join(t.TempDir(), "restore-selection")
	restored, err := Restore(t.Context(), bundle.Path, dir)
	if err != nil {
		t.Fatal(err)
	}
	token, file := generationToken(t)
	request := ActivationRequest{ExpectedInstanceID: restored.InstanceID, FencedPredecessorID: restored.SourceInstanceID, AdminTokenFile: file}
	active, err := Activate(t.Context(), dir, request)
	if err != nil {
		t.Fatal(err)
	}
	return &activationFixture{source: source, dir: dir, token: token, request: request}, ProviderResumptionRequest{ExpectedInstanceID: active.InstanceID, FencedPredecessorID: active.SourceInstanceID, ProviderEffectsSettled: true, AbandonRetainedWork: true, Reason: "Synthetic provider effects settled", EvidenceReference: "private://selection-fixture"}, id
}
func selectionActivationScope(t *testing.T, s *core.Store, f *activationFixture) core.Scope {
	t.Helper()
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("project_id", f.source.project)
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("X-FC-Instance", s.InstanceID())
	r.Header.Set("X-FC-Worker", f.source.worker)
	scope, err := s.AdminScope(r)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}
func TestSelectionResumptionAbandonsPendingAndCannotResurrect(t *testing.T) {
	for _, state := range []string{"queued", "selecting"} {
		t.Run(state, func(t *testing.T) {
			f, in, id := selectionRestoredActive(t, state)
			before := activationRows(t, f.dir)
			original := generationRead(t, f.dir, `SELECT revision,created_at FROM review_selections`)
			summary, err := ResumeProviders(t.Context(), f.dir, in)
			if err != nil || summary.Counts.Selections != 1 || summary.ProvidersHeld {
				t.Fatal("selection release failed", summary, err)
			}
			rows := generationRead(t, f.dir, `SELECT state,reason,revision,operation_id,created_at,updated_at FROM review_selections`)
			if len(rows) != 1 || rows[0][0] != "held" || rows[0][1] != "restore_abandoned" || rows[0][2] != original[0][0].(int64)+1 || rows[0][3] != nil || rows[0][4] != original[0][1] || rows[0][5] != summary.ReleasedAt {
				t.Fatal("pending selection was not retained as terminal abandonment")
			}
			after := activationRows(t, f.dir)
			for _, name := range []string{"receipts", "review_execution_evidence", "review_round_completions", "review_adjudication_state", "review_hosted_allowance_queries"} {
				if !reflect.DeepEqual(before[name], after[name]) {
					t.Fatalf("selection abandonment changed %s", name)
				}
			}
			replay, err := ResumeProviders(t.Context(), f.dir, in)
			if err != nil || replay != summary || !reflect.DeepEqual(after, activationRows(t, f.dir)) {
				t.Fatal("exact release retry was not read-only", err)
			}
			s := activationOpen(t, f.dir)
			e := selectionInventoryEngine(t, s)
			if err = reviewexec.ValidateSelectionSnapshot(t.Context(), s.DB()); err != nil {
				t.Fatal("revoked predecessor provenance became invalid", err)
			}
			if err = e.RecoverReviewRuns(t.Context()); err != nil {
				t.Fatal(err)
			}
			reader := &selectionInventoryReader{state: "selected", entered: make(chan struct{})}
			ctx, cancel := context.WithCancel(t.Context())
			done, err := e.StartReviewRuns(ctx, map[string]reviewexec.TargetReader{"selection-cli": reader})
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			// Recovery and the real worker lifetime must not turn terminal abandonment
			// back into source authority, even with an installed capable reader.
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("owned restored worker did not join")
			}
			if reader.calls.Load() != 0 {
				t.Fatal("abandoned selection invoked source provider")
			}
			got, err := e.ReadRun(t.Context(), selectionActivationScope(t, s, f), id)
			if err != nil || got.State != "held" || got.Reason != "restore_abandoned" || got.OperationID != "" {
				t.Fatal("abandoned run resurrected", err)
			}
			if !reflect.DeepEqual(after, activationRows(t, f.dir)) {
				t.Fatal("read/recovery/restart mutated abandoned selection")
			}
		})
	}
}
func TestSelectionResumptionPreservesSelectedChildAndOldOmittedCount(t *testing.T) {
	f, in, _ := selectionRestoredActive(t, "selected")
	before := generationRead(t, f.dir, `SELECT * FROM review_selections`)
	summary, err := ResumeProviders(t.Context(), f.dir, in)
	if err != nil || summary.Counts.Selections != 0 {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, generationRead(t, f.dir, `SELECT * FROM review_selections`)) {
		t.Fatal("release rewrote terminal selected link")
	}
	op := generationRead(t, f.dir, `SELECT o.state,o.reason FROM review_operations o JOIN review_selections s ON s.operation_id=o.id`)
	if len(op) != 1 || op[0][0] != "held" || op[0][1] != "restore_reconciled" {
		t.Fatal("ordinary selected child was not reconciled through native abandonment")
	}
	release := generationRead(t, f.dir, `SELECT value FROM metadata WHERE key='restore:provider_release'`)[0][0].(string)
	var record struct {
		Counts map[string]json.RawMessage `json:"counts"`
	}
	if json.Unmarshal([]byte(release), &record) != nil || record.Counts["selections"] != nil {
		t.Fatal("zero selection count broke older canonical release shape")
	}
	again, err := ResumeProviders(t.Context(), f.dir, in)
	if err != nil || again != summary {
		t.Fatal("omitted zero count release not replayable", err)
	}
	s := activationOpen(t, f.dir)
	if err = reviewexec.ValidateSelectionSnapshot(t.Context(), s.DB()); err != nil {
		t.Fatal(err)
	}
}
func TestSelectionResumptionFailureRollsBackEarlierAbandonment(t *testing.T) {
	f, in, _ := selectionRestoredActive(t, "queued")
	original := generationRead(t, f.dir, `SELECT id,state FROM review_execution_attempts LIMIT 1`)
	// This preserves canonical DDL/FKs but fails the later native-work guard.
	// An extra trigger would be rejected too early by schema inventory.
	activationExec(t, f.dir, `UPDATE review_execution_attempts SET state='invalid-native-state' WHERE id=?`, original[0][0])
	before := activationRows(t, f.dir)
	db, err := sql.Open("sqlite", filepath.Join(f.dir, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	count, err := reviewexec.AbandonReviewRuns(t.Context(), tx, time.Now().UTC())
	if err != nil || count != 1 {
		t.Fatal("selection phase failed before intended native guard", err)
	}
	var state, reason string
	if err = tx.QueryRow(`SELECT state,reason FROM review_selections`).Scan(&state, &reason); err != nil || state != "held" || reason != "restore_abandoned" {
		t.Fatal("earlier selection write not observable inside transaction", err)
	}
	_, err = reviewexec.AbandonRestoredProviderWork(t.Context(), tx, in.ExpectedInstanceID, time.Now().UTC())
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != "restore_reconciliation_invalid" {
		t.Fatal("native guard did not fail at intended boundary", err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, activationRows(t, f.dir)) {
		t.Fatal("explicit caller rollback retained partial selection/event changes")
	}
	_, err = ResumeProviders(t.Context(), f.dir, in)
	if !errors.As(err, &failure) || failure.Code != "restore_reconciliation_invalid" {
		t.Fatal("actual resumption did not reach native guard", err)
	}
	if !reflect.DeepEqual(before, activationRows(t, f.dir)) {
		t.Fatal("operator transaction retained partial selection/event/release changes")
	}
	activationExec(t, f.dir, `UPDATE review_execution_attempts SET state=? WHERE id=?`, original[0][1], original[0][0])
	out, err := ResumeProviders(t.Context(), f.dir, in)
	if err != nil || out.Counts.Selections != 1 {
		t.Fatal("exact retry did not abandon once", err)
	}
}

func TestSelectionResumptionInvalidAuditCountRefused(t *testing.T) {
	f, in, _ := selectionRestoredActive(t, "queued")
	if _, err := ResumeProviders(t.Context(), f.dir, in); err != nil {
		t.Fatal(err)
	}
	body := generationRead(t, f.dir, `SELECT value FROM metadata WHERE key='restore:provider_release'`)[0][0].(string)
	if !strings.Contains(body, `"selections":1`) {
		t.Fatal("nonzero abandonment count omitted")
	}
	activationExec(t, f.dir, `UPDATE metadata SET value=? WHERE key='restore:provider_release'`, strings.Replace(body, `"selections":1`, `"selections":-1`, 1))
	before := activationRows(t, f.dir)
	if _, err := ResumeProviders(t.Context(), f.dir, in); err == nil || !reflect.DeepEqual(before, activationRows(t, f.dir)) {
		t.Fatal("invalid release count accepted/mutated")
	}
}
