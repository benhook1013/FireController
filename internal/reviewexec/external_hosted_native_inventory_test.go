package reviewexec

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestExternalHostedIncompleteExactNativeInventory(t *testing.T) {
	for _, kind := range []string{"publication", "submitted"} {
		for _, order := range []string{"native_first", "observer_first"} {
			t.Run(kind+"/"+order, func(t *testing.T) {
				f, a, in, external, target := reconciliationFixture(t, kind)
				var oldSnapshot, oldDigest string
				if order == "observer_first" {
					if count, held := reconciliationObserve(t, f, target, external, 1); count != 1 || held {
						t.Fatal("observer fixture", count, held)
					}
					oldSnapshot = reconciliationCurrent(t, f).SnapshotID
					oldDigest = projectionDigest(t, f, oldSnapshot)
				}
				captured, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
				if err != nil {
					t.Fatal(err)
				}
				evidence, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), captured.ID)
				if err != nil {
					t.Fatal(err)
				}
				if order == "observer_first" {
					// Retain the predecessor's duplicate without staging the modern alias.
					run, err := publicationRun(evidence, adjudicationRead(t, f, captured.ID), a)
					if err != nil {
						t.Fatal(err)
					}
					projectionPublish(t, f, reviews.NativePublication{Pull: projectionPull(2), Run: &run})
					if reconciliationCurrent(t, f).Pull.RunCounts.Hosted != 2 {
						t.Fatal("legacy duplicate fixture")
					}
				} else if _, err = f.e.PublishEvidence(t.Context(), attemptProject, captured.ID, cliPublicationPull(2)); err != nil {
					t.Fatal(err)
				}
				adjudicationDecide(t, f, captured.ID, "native-decision-before-mutable-edit", 0, 0, "accepted")
				before := adjudicationRead(t, f, captured.ID)
				beforeAttempt := attemptState(t, f, a)
				beforeDetail := cliPublicationDetail(t, f, 2, captured.ID)
				beforeCredits := attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions`)
				var body string
				if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_evidence WHERE id=?`, captured.ID).Scan(&body); err != nil {
					t.Fatal(err)
				}
				duplicateSnapshot := reconciliationCurrent(t, f).SnapshotID
				duplicateDigest := projectionDigest(t, f, duplicateSnapshot)

				// A later mutable finding edit affects parsing, not immutable ownership.
				external.Inventory.Complete = false
				external.Inventory.Reason = "finding_trailer_unsupported"
				external.Inventory.Findings[0].Title = "Later mutable provider title"
				external.Inventory.Findings[0].Detail = "Later mutable provider text"
				for tick := 0; tick < 2; tick++ {
					if tick == 1 {
						restartIncompleteNativeInventory(t, f)
					}
					if count, held := reconciliationObserve(t, f, target, external, 1); count != 0 || held {
						t.Fatal("exact captured native identity held or imported", count, held)
					}
					if reconciliationCurrent(t, f).Pull.RunCounts.Hosted != 1 {
						t.Fatal("exact native publication was not counted once")
					}
					afterDetail := cliPublicationDetail(t, f, 2, captured.ID)
					if !reflect.DeepEqual(before, adjudicationRead(t, f, captured.ID)) || beforeAttempt != attemptState(t, f, a) || !reflect.DeepEqual(beforeDetail.Run, afterDetail.Run) || !reflect.DeepEqual(beforeDetail.Findings, afterDetail.Findings) || beforeCredits != attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions`) {
						t.Fatal("mutable observation changed native findings, decisions, execution or credit")
					}
					var afterBody string
					if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_evidence WHERE id=?`, captured.ID).Scan(&afterBody); err != nil || afterBody != body {
						t.Fatal("immutable evidence/capture reference changed", err)
					}
					if projectionDigest(t, f, duplicateSnapshot) != duplicateDigest || oldSnapshot != "" && projectionDigest(t, f, oldSnapshot) != oldDigest {
						t.Fatal("historical snapshot changed")
					}
					if tick == 0 {
						duplicateSnapshot = reconciliationCurrent(t, f).SnapshotID
						duplicateDigest = projectionDigest(t, f, duplicateSnapshot)
					} else if reconciliationCurrent(t, f).SnapshotID != duplicateSnapshot {
						t.Fatal("restart/replay appended another snapshot")
					}
				}
			})
		}
	}
}

func restartIncompleteNativeInventory(t *testing.T, f *attemptFixture) {
	t.Helper()
	var sequence int
	var name, path string
	if err := f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	instance := f.store.InstanceID()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(t.Context(), filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f.store = store
	if store.InstanceID() != instance {
		t.Fatal("restart changed instance")
	}
	f.e, err = New(store, attemptCapabilities, WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	reviews.Register(mux, store)
	f.handler = mux
}

func TestExternalHostedIncompleteNativeInventoryRequiresExactCapture(t *testing.T) {
	for _, mode := range []string{"head", "review_subset", "provider_attempt", "source_id", "unknown", "forged_marker", "capture_only", "known_identity_conflict", "zero_finish", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			f, a, in, external, target := reconciliationFixture(t, "publication")
			if mode != "unknown" && mode != "forged_marker" {
				if count, held := reconciliationObserve(t, f, target, external, 1); count != 1 || held {
					t.Fatal("observer fixture", count, held)
				}
				captured, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
				if err != nil {
					t.Fatal(err)
				}
				if mode != "capture_only" {
					evidence, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), captured.ID)
					if err != nil {
						t.Fatal(err)
					}
					run, err := publicationRun(evidence, adjudicationRead(t, f, captured.ID), a)
					if err != nil {
						t.Fatal(err)
					}
					projectionPublish(t, f, reviews.NativePublication{Pull: projectionPull(2), Run: &run})
				}
			}
			external.Inventory.Complete = false
			external.Inventory.Reason = "root_binding_mismatch"
			switch mode {
			case "head":
				external.HeadSHA = strings.Repeat("e", 40)
			case "review_subset":
				external.ReviewIDs = external.ReviewIDs[:len(external.ReviewIDs)-1]
			case "provider_attempt":
				external.Inventory.Publication.ProviderAttemptID = "99999999-9999-4999-8999-999999999999"
			case "source_id":
				external.SourceID = "publication:99999999-9999-4999-8999-999999999999"
			case "forged_marker":
				// Even a matching active operation marker cannot waive inventory proof.
				external.NativeOperationID, external.TriggerCommentID = a.OperationID, 101
			case "known_identity_conflict":
				admissionSQL(t, f, `UPDATE review_run_versions SET head_sha=? WHERE source='github-hosted-observer'`, strings.Repeat("e", 40))
			case "zero_finish":
				external.FinishedAt = time.Time{}
			case "oversized":
				external.Inventory.Findings = make([]reviewprovider.HostedFinding, 101)
			}
			before := projectionDigest(t, f, "")
			// Exact capture-only ownership may skip import, but cannot close the
			// observer membership before a current native replacement exists.
			if count, held := reconciliationObserve(t, f, target, external, 1); count != 0 || held != (mode != "capture_only") {
				t.Fatal("unproven incomplete source escaped hold", count, held)
			}
			if projectionDigest(t, f, "") != before {
				t.Fatal("held source changed retained history")
			}
		})
	}
}

func TestExternalHostedIncompleteNativeInventoryKeepsAppendCapacity(t *testing.T) {
	f, a, in, external, target := reconciliationFixture(t, "publication")
	captured, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.PublishEvidence(t.Context(), attemptProject, captured.ID, cliPublicationPull(2)); err != nil {
		t.Fatal(err)
	}
	next := external
	next.SourceID = "publication:99999999-9999-4999-8999-999999999999"
	next.Inventory.Publication.PublicationID = "99999999-9999-4999-8999-999999999999"
	next.ReviewIDs = []int64{999}
	external.Inventory.Complete = false
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		inputs, held, err := f.e.externalHostedInputs(ctx, tx, target, []reviewprovider.ExternalHostedRun{external, external, next}, 1)
		if err == nil && (held || len(inputs) != 1 || inputs[0].SourceRunID != next.SourceID) {
			return fmt.Errorf("exact incomplete aliases consumed capacity or held independent inventory: %d/%t", len(inputs), held)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExternalHostedIncompleteNativeInventoryCaptureReadFailure(t *testing.T) {
	for _, mode := range []string{"capture_read", "zero_finish", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			f, _, _, external, target := reconciliationFixture(t, "publication")
			external.Inventory.Complete = false
			// Force the existing indexed lookup to fail without touching provider data.
			admissionSQL(t, f, `DROP INDEX review_evidence_hosted_publication`)
			switch mode {
			case "zero_finish":
				external.FinishedAt = time.Time{}
			case "oversized":
				external.Inventory.Findings = make([]reviewprovider.HostedFinding, 101)
			}
			before := projectionDigest(t, f, "")
			readFailed := false
			err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				inputs, held, err := f.e.externalHostedInputs(ctx, tx, target, []reviewprovider.ExternalHostedRun{external}, 1)
				if mode == "capture_read" {
					readFailed = err != nil
					return err
				}
				if err != nil {
					return err
				}
				if len(inputs) != 0 || !held {
					return fmt.Errorf("cheap guard failed: %d/%t", len(inputs), held)
				}
				return nil
			})
			if mode == "capture_read" && (!readFailed || err == nil) || mode != "capture_read" && err != nil {
				t.Fatal("capture failure/cheap guard boundary changed", err)
			}
			if projectionDigest(t, f, "") != before {
				t.Fatal("capture read failure changed retained history")
			}
		})
	}
}
