package reviewexec

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func hostedRecoveryFixture(t *testing.T) (*attemptFixture, Attempt, time.Time) {
	t.Helper()
	f, a, _ := hostedObservationFixture(t)
	// Both records occur in one second, but GitHub only retains that second.
	// The operation really precedes admission even though the returned floor
	// necessarily precedes its nanosecond creation time.
	second := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Second)
	admitted := second.Add(700 * time.Millisecond)
	admissionSQL(t, f, `UPDATE review_operations SET created_at=? WHERE id=?`, second.Add(100*time.Millisecond).Format(time.RFC3339Nano), a.OperationID)
	admissionSQL(t, f, `UPDATE review_execution_transitions SET created_at=? WHERE attempt_id=? AND revision=1`, second.Add(200*time.Millisecond).Format(time.RFC3339Nano), a.ID)
	admissionSQL(t, f, `UPDATE review_execution_transitions SET created_at=? WHERE attempt_id=? AND revision=2`, admitted.Format(time.RFC3339Nano), a.ID)
	return f, a, admitted
}

func TestReadHostedRecoveryRetainsPostingBoundaryAndCopiesAcrossStates(t *testing.T) {
	for _, state := range []string{"posting", "active", "startup-uncertain", "completed"} {
		t.Run(state, func(t *testing.T) {
			f, a, admitted := hostedRecoveryFixture(t)
			var err error
			if state == "active" || state == "completed" {
				a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "posted", "101")
				if err != nil {
					t.Fatal(err)
				}
			}
			if state == "completed" {
				a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "captured", "")
				if err != nil {
					t.Fatal(err)
				}
			}
			if state == "startup-uncertain" {
				if err := f.e.RecoverInterrupted(t.Context()); err != nil {
					t.Fatal(err)
				}
				a.State = "uncertain"
			}
			before := hostedObservationState(t, f, a)
			got, err := f.e.ReadHostedRecovery(t.Context(), attemptProject, a.ID, "test-capability")
			if err != nil || got.Operation.ID != a.OperationID || got.Operation.ProjectID != attemptProject || got.Attempt.ID != a.ID || got.Operation.State != a.State || got.Attempt.State != a.State || got.Attempt.Intent != a.Intent || got.Attempt.Anchor == nil || *got.Attempt.Anchor != *a.Anchor || got.EarliestPosting != admitted.Truncate(time.Second) || got.EvidenceID != "" {
				t.Fatalf("retained recovery context: %+v %v", got, err)
			}
			exact, err := f.e.ReadHostedPostingBoundary(t.Context(), attemptProject, a.ID)
			if err != nil || !exact.Equal(admitted) || exact.Equal(got.EarliestPosting) || hostedObservationState(t, f, a) != before {
				t.Fatal("exact local boundary lost precision or changed journal", exact, err)
			}
			if got.EarliestPosting.Format(time.RFC3339Nano) == a.Anchor.ObservedAt {
				t.Fatal("source observation replaced posting admission")
			}
			original := *got.Attempt.Anchor
			got.Attempt.Anchor.Repository = "caller/changed"
			got.Operation.HeadSHA = "caller changed"
			replay, err := f.e.ReadHostedRecovery(t.Context(), attemptProject, a.ID, "test-capability")
			if err != nil || *replay.Attempt.Anchor != original || replay.Operation.HeadSHA != original.CandidateSHA || hostedObservationState(t, f, a) != before {
				t.Fatal("recovery alias/replay mutated journal", err)
			}
		})
	}
}

func TestReadHostedRecoveryObservesAfterRevocationPauseAndReconfiguration(t *testing.T) {
	f, a, admitted := hostedRecoveryFixture(t)
	f.configure(t, attemptProject, "other-account", 1, true)
	admissionPolicy(t, f, 1, "hosted")
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
	admissionSQL(t, f, `DELETE FROM grants WHERE credential_id='exec-writer-cred'`)
	// A repository spelling change does not rewrite the original immutable anchor.
	admissionSQL(t, f, `UPDATE projects SET repository='renamed/repository' WHERE id=?`, attemptProject)
	before := hostedObservationState(t, f, a)
	got, err := f.e.ReadHostedRecovery(t.Context(), attemptProject, a.ID, "test-capability")
	if err != nil || got.Operation.ConfigurationRevision != 1 || got.Operation.PolicyRevision != 1 || got.Attempt.Anchor.Repository != a.Anchor.Repository || got.EarliestPosting != admitted.Truncate(time.Second) || hostedObservationState(t, f, a) != before {
		t.Fatal("new admission eligibility blocked observation of retained work", got, err)
	}
}

func TestReadHostedRecoveryRejectsWrongStateScopeAndCapability(t *testing.T) {
	for name, change := range map[string]func(*testing.T, *attemptFixture, Attempt){
		"preparing": func(t *testing.T, f *attemptFixture, a Attempt) {
			admissionSQL(t, f, `UPDATE review_execution_attempts SET state='preparing' WHERE id=?`, a.ID)
			admissionSQL(t, f, `UPDATE review_operations SET state='preparing' WHERE id=?`, a.OperationID)
		},
		"failed": func(t *testing.T, f *attemptFixture, a Attempt) {
			if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "failed", "failed", ""); err != nil {
				t.Fatal(err)
			}
		},
		"different operation state": func(t *testing.T, f *attemptFixture, a Attempt) {
			admissionSQL(t, f, `UPDATE review_operations SET state='active' WHERE id=?`, a.OperationID)
		},
		"different attempt binding": func(t *testing.T, f *attemptFixture, a Attempt) {
			admissionSQL(t, f, `UPDATE review_operations SET attempt_id='' WHERE id=?`, a.OperationID)
		},
		"CLI": func(t *testing.T, f *attemptFixture, a Attempt) {
			admissionSQL(t, f, `UPDATE review_operations SET channel='cli' WHERE id=?`, a.OperationID)
		},
		"repository identity": func(t *testing.T, f *attemptFixture, a Attempt) {
			admissionSQL(t, f, `UPDATE projects SET github_repository_id=999 WHERE id=?`, attemptProject)
		},
		"token fingerprint": func(t *testing.T, f *attemptFixture, a Attempt) {
			var err error
			f.e, err = New(f.store, []Capability{{ID: "test-capability", Provider: "coderabbit", Account: "test-account", Channels: []string{"cli", "hosted"}, Fingerprint: strings.Repeat("f", 64)}})
			if err != nil {
				t.Fatal(err)
			}
		},
		"current provider": func(t *testing.T, f *attemptFixture, a Attempt) {
			var err error
			f.e, err = New(f.store, []Capability{{ID: "test-capability", Provider: "different-provider", Account: "test-account", Channels: []string{"cli", "hosted"}}})
			if err != nil {
				t.Fatal(err)
			}
		},
		"current account": func(t *testing.T, f *attemptFixture, a Attempt) {
			var err error
			f.e, err = New(f.store, []Capability{{ID: "test-capability", Provider: "coderabbit", Account: "different-account", Channels: []string{"cli", "hosted"}}})
			if err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, a, _ := hostedRecoveryFixture(t)
			change(t, f, a)
			before := hostedObservationState(t, f, a)
			got, err := f.e.ReadHostedRecovery(t.Context(), attemptProject, a.ID, "test-capability")
			attemptCode(t, err, "review_conflict")
			if !reflect.DeepEqual(got, HostedRecoveryContext{}) || hostedObservationState(t, f, a) != before {
				t.Fatal("refused recovery returned facts or mutated state")
			}
		})
	}
	f, a, _ := hostedRecoveryFixture(t)
	_, err := f.e.ReadHostedRecovery(t.Context(), otherAttemptProject, a.ID, "test-capability")
	attemptCode(t, err, "not_found")
	_, err = f.e.ReadHostedRecovery(t.Context(), attemptProject, a.ID, "other-account")
	attemptCode(t, err, "review_conflict")
	// Absence and wrong channel fail before even opening a local DB snapshot.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = f.e.ReadHostedRecovery(t.Context(), attemptProject, a.ID, "missing-capability")
	attemptCode(t, err, "review_conflict")
	f.e.capabilities["test-capability"] = Capability{ID: "test-capability", Channels: []string{"cli"}}
	_, err = f.e.ReadHostedRecovery(t.Context(), attemptProject, a.ID, "test-capability")
	attemptCode(t, err, "review_conflict")
}

func TestReadHostedRecoveryRejectsCorruptAdmissionFacts(t *testing.T) {
	for name, change := range map[string]func(*testing.T, *attemptFixture, Attempt, time.Time){
		"missing admitted": func(t *testing.T, f *attemptFixture, a Attempt, _ time.Time) {
			admissionSQL(t, f, `DELETE FROM review_execution_transitions WHERE attempt_id=? AND revision=2`, a.ID)
		},
		"duplicate admitted": func(t *testing.T, f *attemptFixture, a Attempt, at time.Time) {
			admissionSQL(t, f, `INSERT INTO review_execution_transitions(attempt_id,revision,state,reason,external_id,created_at) VALUES(?,99,'posting','admitted','',?)`, a.ID, at.Format(time.RFC3339Nano))
		},
		"admitted revision": func(t *testing.T, f *attemptFixture, a Attempt, _ time.Time) {
			admissionSQL(t, f, `UPDATE review_execution_transitions SET revision=99 WHERE attempt_id=? AND revision=2`, a.ID)
		},
		"admitted state": func(t *testing.T, f *attemptFixture, a Attempt, _ time.Time) {
			admissionSQL(t, f, `UPDATE review_execution_transitions SET state='active' WHERE attempt_id=? AND revision=2`, a.ID)
		},
		"admitted external id": func(t *testing.T, f *attemptFixture, a Attempt, _ time.Time) {
			admissionSQL(t, f, `UPDATE review_execution_transitions SET external_id='101' WHERE attempt_id=? AND revision=2`, a.ID)
		},
		"timestamp syntax": func(t *testing.T, f *attemptFixture, a Attempt, _ time.Time) {
			admissionSQL(t, f, `UPDATE review_execution_transitions SET created_at='private invalid timestamp' WHERE attempt_id=? AND revision=2`, a.ID)
		},
		"timestamp offset": func(t *testing.T, f *attemptFixture, a Attempt, at time.Time) {
			admissionSQL(t, f, `UPDATE review_execution_transitions SET created_at=? WHERE attempt_id=? AND revision=2`, at.Format("2006-01-02T15:04:05.999999999-07:00"), a.ID)
		},
		"admission before operation": func(t *testing.T, f *attemptFixture, a Attempt, at time.Time) {
			admissionSQL(t, f, `UPDATE review_execution_transitions SET created_at=? WHERE attempt_id=? AND revision=2`, at.Add(-time.Second).Format(time.RFC3339Nano), a.ID)
		},
		"operation timestamp": func(t *testing.T, f *attemptFixture, a Attempt, _ time.Time) {
			admissionSQL(t, f, `UPDATE review_operations SET created_at='bad' WHERE id=?`, a.OperationID)
		},
		"intent corruption": func(t *testing.T, f *attemptFixture, a Attempt, _ time.Time) {
			admissionSQL(t, f, `UPDATE review_execution_attempts SET intent='@coderabbitai review' WHERE id=?`, a.ID)
		},
		"missing anchor": func(t *testing.T, f *attemptFixture, a Attempt, _ time.Time) {
			admissionSQL(t, f, `UPDATE review_execution_attempts SET anchor_json='null' WHERE id=?`, a.ID)
		},
		"malformed anchor": func(t *testing.T, f *attemptFixture, a Attempt, _ time.Time) {
			admissionSQL(t, f, `UPDATE review_execution_attempts SET anchor_json='private bad json' WHERE id=?`, a.ID)
		},
		"mismatched anchor": func(t *testing.T, f *attemptFixture, a Attempt, _ time.Time) {
			anchor := *a.Anchor
			anchor.CandidateSHA = strings.Repeat("f", 40)
			admissionSQL(t, f, `UPDATE review_execution_attempts SET anchor_json=? WHERE id=?`, encoded(anchor), a.ID)
		},
		"missing fingerprint": func(t *testing.T, f *attemptFixture, a Attempt, _ time.Time) {
			admissionSQL(t, f, `UPDATE review_operations SET capability_fingerprint='' WHERE id=?`, a.OperationID)
		},
		"null expected configuration revision": func(t *testing.T, f *attemptFixture, a Attempt, _ time.Time) {
			var body string
			if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_config WHERE project_id=? AND revision=1`, attemptProject).Scan(&body); err != nil {
				t.Fatal(err)
			}
			var c Configuration
			if err := json.Unmarshal([]byte(body), &c); err != nil {
				t.Fatal(err)
			}
			c.ExpectedRevision = nil
			admissionSQL(t, f, `UPDATE review_execution_config SET body_json=? WHERE project_id=? AND revision=1`, encoded(c), attemptProject)
		},
		"retained config": func(t *testing.T, f *attemptFixture, a Attempt, _ time.Time) {
			admissionSQL(t, f, `UPDATE review_execution_config SET body_json='private invalid config' WHERE project_id=? AND revision=1`, attemptProject)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, a, at := hostedRecoveryFixture(t)
			change(t, f, a, at)
			before := hostedObservationState(t, f, a)
			got, err := f.e.ReadHostedRecovery(t.Context(), attemptProject, a.ID, "test-capability")
			attemptCode(t, err, "storage_unavailable")
			if !reflect.DeepEqual(got, HostedRecoveryContext{}) || strings.Contains(err.Error(), "private") || hostedObservationState(t, f, a) != before {
				t.Fatal("corrupt admission escaped/repaired/leaked", got, err)
			}
			if name != "retained config" {
				exact, err := f.e.ReadHostedPostingBoundary(t.Context(), attemptProject, a.ID)
				attemptCode(t, err, "storage_unavailable")
				if !exact.IsZero() || hostedObservationState(t, f, a) != before {
					t.Fatal("corrupt exact boundary escaped or mutated journal", exact, err)
				}
			}
		})
	}
}

func TestReadHostedRecoveryReturnsRetainedFinalEvidenceWithoutCredit(t *testing.T) {
	f, a, input := hostedPublicationFixture(t, false)
	receipt, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	before := hostedObservationState(t, f, a)
	got, err := f.e.ReadHostedRecovery(t.Context(), attemptProject, a.ID, "test-capability")
	if err != nil || got.EvidenceID != receipt.ID || hostedObservationState(t, f, a) != before {
		t.Fatal("retained unfinalized evidence not returned for publication recovery", got, err)
	}
	// Completed attempts remain recoverable; there is no requirement for an active
	// reservation or finalized source before retrying the local publication.
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "capture_retained", ""); err != nil {
		t.Fatal(err)
	}
	before = hostedObservationState(t, f, a)
	got, err = f.e.ReadHostedRecovery(t.Context(), attemptProject, a.ID, "test-capability")
	if err != nil || got.EvidenceID != receipt.ID || hostedObservationState(t, f, a) != before {
		t.Fatal("completed final evidence became unavailable", got, err)
	}
}

func TestReadHostedRecoveryRejectsCorruptFinalEvidence(t *testing.T) {
	for name, change := range map[string]func(*ReviewEvidence){
		"attempt":        func(e *ReviewEvidence) { e.AttemptID = "review_attempt_" + strings.Repeat("f", 32) },
		"operation":      func(e *ReviewEvidence) { e.OperationID = "review_operation_" + strings.Repeat("f", 32) },
		"fingerprint":    func(e *ReviewEvidence) { e.CapabilityFingerprint = strings.Repeat("f", 64) },
		"anchor":         func(e *ReviewEvidence) { e.Anchor.ParentSHA = strings.Repeat("f", 40) },
		"capture anchor": func(e *ReviewEvidence) { e.Capture.ExpectedAnchor.ParentSHA = strings.Repeat("f", 40) },
		"CLI capture": func(e *ReviewEvidence) {
			e.Capture.Hosted = nil
			e.Capture.Review.ReviewedFiles = []string{"source.go"}
		},
		"posted identity": func(e *ReviewEvidence) { e.Capture.Hosted.TriggerCommentID++ },
		"metadata":        func(e *ReviewEvidence) { e.Capture.Hosted.PublicationID = "bad" },
	} {
		t.Run(name, func(t *testing.T) {
			f, a, input := hostedPublicationFixture(t, true)
			r, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, input)
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), r.ID)
			if err != nil {
				t.Fatal(err)
			}
			change(&evidence)
			body, _ := json.Marshal(evidence)
			admissionSQL(t, f, `UPDATE review_execution_evidence SET body_json=? WHERE id=?`, string(body), r.ID)
			before := hostedObservationState(t, f, a)
			got, err := f.e.ReadHostedRecovery(t.Context(), attemptProject, a.ID, "test-capability")
			attemptCode(t, err, "storage_unavailable")
			if !reflect.DeepEqual(got, HostedRecoveryContext{}) || hostedObservationState(t, f, a) != before {
				t.Fatal("corrupt final evidence used for recovery")
			}
		})
	}
	for _, column := range []string{"project_id", "operation_id"} {
		t.Run("indexed-"+column, func(t *testing.T) {
			f, a, input := hostedPublicationFixture(t, true)
			r, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, input)
			if err != nil {
				t.Fatal(err)
			}
			foreign := otherAttemptProject
			if column == "operation_id" {
				foreign = f.enqueue(t, attemptProject, "other-operation", "hosted", 4, 1).ID
			}
			admissionSQL(t, f, "UPDATE review_execution_evidence SET "+column+"=? WHERE id=?", foreign, r.ID)
			before := hostedObservationState(t, f, a)
			_, err = f.e.ReadHostedRecovery(t.Context(), attemptProject, a.ID, "test-capability")
			attemptCode(t, err, "storage_unavailable")
			if hostedObservationState(t, f, a) != before {
				t.Fatal("indexed corruption changed retained state")
			}
		})
	}
}

func TestReadHostedRecoveryRejectsDuplicateRetainedEvidence(t *testing.T) {
	f, a, input := hostedPublicationFixture(t, true)
	if _, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, input); err != nil {
		t.Fatal(err)
	}
	// Simulate a corrupt retained schema that lost the unique-attempt constraint.
	// This is an isolated SQLite fixture, never a migration or runtime repair.
	admissionSQL(t, f, `ALTER TABLE review_execution_evidence RENAME TO recovery_original_evidence`)
	admissionSQL(t, f, `CREATE TABLE review_execution_evidence AS SELECT * FROM recovery_original_evidence`)
	admissionSQL(t, f, `INSERT INTO review_execution_evidence(id,project_id,attempt_id,operation_id,body_json,fingerprint) SELECT ?,project_id,attempt_id,operation_id,body_json,fingerprint FROM recovery_original_evidence`, "review_evidence_"+strings.Repeat("f", 32))
	before := hostedObservationState(t, f, a)
	got, err := f.e.ReadHostedRecovery(t.Context(), attemptProject, a.ID, "test-capability")
	attemptCode(t, err, "storage_unavailable")
	if !reflect.DeepEqual(got, HostedRecoveryContext{}) || hostedObservationState(t, f, a) != before {
		t.Fatal("ambiguous final evidence selected or repaired")
	}
}
