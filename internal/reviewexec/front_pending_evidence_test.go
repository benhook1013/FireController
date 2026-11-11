package reviewexec

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func pendingFrontFrame(t *testing.T, f *attemptFixture, plan SourcePlan) {
	t.Helper()
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	c, revision, err := readConfiguration(t.Context(), tx, attemptProject)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeFrontFrame(t.Context(), tx, Run{ProjectID: attemptProject, Channel: "cli", ConfigurationRevision: revision}, plan.Repository.ID, c, runChoice{prefix: []SourcePlan{plan}}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func pendingFrontRead(t *testing.T, f *attemptFixture) FrontChannel {
	t.Helper()
	v, err := f.e.ReadFronts(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader))
	if err != nil {
		t.Fatal(err)
	}
	return frontChannel(t, v, "cli")
}

func TestFrontPendingFixesStagesAndAdmissionUnchanged(t *testing.T) {
	f, a, id, plan := coverageFixture(t, 1, false)
	adjudicationComplete(t, f, a)
	pendingFrontFrame(t, f, plan)
	check := func(want string) {
		t.Helper()
		before := adjudicationState(t, f, a) + snapshotTable(t, f, "review_comparison_proofs")
		got := pendingFrontRead(t, f)
		if got.State != "held" || got.Reason != want || got.PullNumber != 2 {
			t.Fatal(got)
		}
		if err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			result, err := f.e.evaluateFront(ctx, tx, Run{ProjectID: attemptProject, Channel: "cli"}, 801, []SourcePlan{plan}, false)
			if err == nil && (result.state != "held" || result.reason != "review_evidence_pending") {
				t.Fatal("display specialization escaped into admission", result)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if after := adjudicationState(t, f, a) + snapshotTable(t, f, "review_comparison_proofs"); before != after {
			t.Fatal("front read changed durable state")
		}
	}
	check("review_evidence_pending")
	adjudicationDecide(t, f, id, "front-accept", 0, 0, "accepted")
	check("review_evidence_pending")
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "front-finalize", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)})
	adjudicationReceipt(t, body, err, id, "finalized", 2)
	check("review_fixes_pending")
	coverageFix(t, f, id, "front-fix", 2, 0, "")
	if got := pendingFrontRead(t, f); got.Reason == "review_fixes_pending" || got.Reason == "review_evidence_pending" {
		t.Fatal("fixed evidence still pending", got)
	}
	coverageRead(t, f, a, plan, true)
}

func TestFrontPendingFixesIncompleteAndMissingCapture(t *testing.T) {
	t.Run("incomplete", func(t *testing.T) {
		f, a, _, plan := coverageFixture(t, 1, true)
		adjudicationComplete(t, f, a)
		pendingFrontFrame(t, f, plan)
		if got := pendingFrontRead(t, f); got.Reason != "review_evidence_pending" {
			t.Fatal(got)
		}
	})
	t.Run("missing", func(t *testing.T) {
		f, a, id, plan := coverageFixture(t, 1, false)
		adjudicationDecide(t, f, id, "front-accept", 0, 0, "accepted")
		coverageFinalize(t, f, a, id, 1)
		pendingFrontFrame(t, f, plan)
		b := f.start(t, attemptProject, f.enqueue(t, attemptProject, "front-missing", "cli", 2, 1))
		anchor := attemptAnchor(attemptProject, 2)
		anchor.ParentSHA = anchor.BaseSHA
		var err error
		b, err = f.e.Admit(t.Context(), attemptProject, b.ID, b.Revision, anchor, "")
		if err != nil {
			t.Fatal(err)
		}
		adjudicationComplete(t, f, b)
		if got := pendingFrontRead(t, f); got.Reason != "review_evidence_pending" {
			t.Fatal("missing capture concealed by finalized evidence", got)
		}
	})
}

func TestFrontPendingFixesScopeAndMixedEvidence(t *testing.T) {
	f, a, id, plan := coverageFixture(t, 1, false)
	adjudicationDecide(t, f, id, "front-accept", 0, 0, "accepted")
	coverageFinalize(t, f, a, id, 1)
	pendingFrontFrame(t, f, plan)
	for _, scope := range []struct {
		project string
		pull    int64
		channel string
		want    bool
	}{
		{attemptProject, 2, "cli", true}, {otherAttemptProject, 2, "cli", false}, {attemptProject, 4, "cli", false}, {attemptProject, 2, "hosted", false},
	} {
		got, err := frontPendingFixes(t.Context(), f.store.DB(), scope.project, scope.pull, scope.channel)
		if err != nil || got != scope.want {
			t.Fatal(scope, got, err)
		}
	}
	// A second complete native result still awaiting adjudication must prevent
	// a fixes-only label, even though the first result is finalized.
	evidence, _, err := loadReviewAdjudication(t.Context(), f.store.DB(), attemptProject, id)
	if err != nil {
		t.Fatal(err)
	}
	b := f.start(t, attemptProject, f.enqueue(t, attemptProject, "front-second", "cli", 2, 1))
	b, err = f.e.Admit(t.Context(), attemptProject, b.ID, b.Revision, evidence.Anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	capture := evidence.Capture
	capture.CaptureSHA256 = strings.Repeat("2", 64)
	capture.CaptureID = "review_capture_" + capture.CaptureSHA256[:32]
	if _, err = f.e.RecordReviewEvidence(t.Context(), attemptProject, b.ID, capture); err != nil {
		t.Fatal(err)
	}
	adjudicationComplete(t, f, b)
	if got := pendingFrontRead(t, f); got.Reason != "review_evidence_pending" {
		t.Fatal("mixed obligations mislabeled", got)
	}
}

func TestFrontPendingFixesDirtyRemainsHeld(t *testing.T) {
	for _, statement := range []string{
		`UPDATE review_comparison_proofs SET state='dirty'`,
		`UPDATE review_adjudication_state SET body_json='{}'`,
		`UPDATE review_execution_evidence SET body_json='{}'`,
	} {
		t.Run(statement, func(t *testing.T) {
			f, a, id, plan := coverageFixture(t, 1, false)
			adjudicationDecide(t, f, id, "front-accept", 0, 0, "accepted")
			coverageFinalize(t, f, a, id, 1)
			pendingFrontFrame(t, f, plan)
			admissionSQL(t, f, statement)
			got := pendingFrontRead(t, f)
			if got.State != "held" || got.Reason == "review_fixes_pending" {
				t.Fatal("dirty source relabeled as fixes", got)
			}
		})
	}
}

func TestFrontPendingFixesQueryIsScopedAndCompact(t *testing.T) {
	f, a, id, _ := coverageFixture(t, 1, false)
	adjudicationDecide(t, f, id, "front-accept", 0, 0, "accepted")
	coverageFinalize(t, f, a, id, 1)
	rows, err := f.store.DB().Query("EXPLAIN QUERY PLAN "+frontPendingFixesSQL, attemptProject, 2, "cli")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details.WriteString(detail + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := details.String()
	if !strings.Contains(plan, "SEARCH p USING INDEX review_comparison_proofs_") || !strings.Contains(plan, "project_id=? AND pull_number=? AND channel=? AND state=?") || !strings.Contains(plan, "SEARCH s USING INDEX") || strings.Contains(plan, "SCAN ") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatal(plan)
	}
}

func TestFrontPendingFixesRequiresBooleanFinalization(t *testing.T) {
	for _, body := range []string{`{}`, `{"finalized":null}`, `{"finalized":false}`, `{"finalized":1}`, `{"finalized":"true"}`, `{broken`} {
		t.Run(body, func(t *testing.T) {
			f, a, id, _ := coverageFixture(t, 1, false)
			adjudicationDecide(t, f, id, "front-accept", 0, 0, "accepted")
			coverageFinalize(t, f, a, id, 1)
			admissionSQL(t, f, `UPDATE review_adjudication_state SET body_json=? WHERE evidence_id=?`, body, id)
			// Exercise the compact query's defensive handling independently of the
			// dirty-proof fence covered above. Production source mutations stay dirty.
			admissionSQL(t, f, `UPDATE review_comparison_proofs SET state='pending'`)
			got, err := frontPendingFixes(t.Context(), f.store.DB(), attemptProject, 2, "cli")
			if body == `{broken` {
				if err == nil {
					t.Fatal("malformed JSON silently classified")
				}
				return
			}
			if err != nil || got {
				t.Fatal("non-boolean finalization supplied fixes-only proof", got, err)
			}
		})
	}
}
