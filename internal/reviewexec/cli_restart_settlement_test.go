package reviewexec

import (
	"testing"
)

func restartedEngine(t *testing.T, f *attemptFixture, restartProof bool) *Engine {
	t.Helper()
	options := []EngineOption{WithNativeProjection()}
	if restartProof {
		options = append(options, WithRestartEndsCLIProcesses())
	}
	e, err := New(f.store, attemptCapabilities, options...)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestRestartSettlesInterruptedCLIWithoutCredit(t *testing.T) {
	for _, outcome := range []string{"", "failed"} {
		t.Run("evidence_"+outcome, func(t *testing.T) {
			var f *attemptFixture
			var a Attempt
			if outcome == "" {
				var input ReviewEvidenceInput
				f, a, input = evidenceFixture(t, "cli", false)
				input.ExpectedAnchor.ParentSHA = input.ExpectedAnchor.BaseSHA
				var err error
				if a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, input.ExpectedAnchor, ""); err != nil {
					t.Fatal(err)
				}
			} else {
				f, a, _ = cliPublicationFixture(t, 0, outcome)
			}
			f.e = restartedEngine(t, f, true)
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=? AND execution_ended_at<>''`, a.ID) != 1 {
				t.Fatal("restart did not free execution capacity")
			}
			pending, err := f.e.RestartedCLIAttempts(t.Context(), "test-capability", "")
			if err != nil || len(pending) != 1 || pending[0].AttemptID != a.ID || pending[0].HasEvidence != (outcome != "") {
				t.Fatal("restarted attempt not listed", pending, err)
			}
			settled, err := f.e.SettleRestartedCLI(t.Context(), pending[0])
			if err != nil || settled.State != "failed" {
				t.Fatal("settlement failed", settled, err)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 0 {
				t.Fatal("PR reservation retained")
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_operations WHERE attempt_id=? AND state='failed' AND reason='cli_restart_settled'`, a.ID) != 1 {
				t.Fatal("operation not settled")
			}
			want := 0
			if outcome != "" {
				want = 1
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, a.ID) != want {
				t.Fatal("evidence changed")
			}
			if _, err := f.e.SettleRestartedCLI(t.Context(), pending[0]); err == nil {
				t.Fatal("replayed settlement accepted")
			}
		})
	}
}

func TestRestartKeepsCompletedAndUnprovenCLI(t *testing.T) {
	f, a, _ := cliPublicationFixture(t, 0, "completed")
	f.e = restartedEngine(t, f, true)
	pending, err := f.e.RestartedCLIAttempts(t.Context(), "test-capability", "")
	if err != nil || len(pending) != 0 {
		t.Fatal("complete result offered for failure settlement", pending, err)
	}
	if _, err := f.e.SettleRestartedCLI(t.Context(), RestartedCLIAttempt{ProjectID: a.ProjectID, AttemptID: a.ID, Revision: a.Revision + 1}); err == nil {
		t.Fatal("complete result settled as failed")
	}

	g, b, _ := cliPublicationFixture(t, 0, "failed")
	g.e = restartedEngine(t, g, false)
	if attemptCount(t, g.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=? AND execution_ended_at=''`, b.ID) != 1 {
		t.Fatal("capacity released without restart proof")
	}
	pending, err = g.e.RestartedCLIAttempts(t.Context(), "test-capability", "")
	if err != nil || len(pending) != 0 {
		t.Fatal("unproven restart listed", pending, err)
	}
}
