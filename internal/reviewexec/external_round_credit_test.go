package reviewexec

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestExternalRoundCreditSharesNativeOrdinalsAndRecognizesLaterAlias(t *testing.T) {
	for _, alias := range []bool{false, true} {
		name := "distinct-native-round"
		if alias {
			name = "exact-native-alias"
		}
		t.Run(name, func(t *testing.T) {
			f, schedule, _ := lifecycleFixture(t)
			roundGrant(t, f, 2, "hosted", 0, 0, 3, "manual-round-budget")
			lifecycleTick(t, f, schedule, externalReader(externalSnapshot(1, strings.Repeat("a", 40))))
			var history reviews.RunsResponse
			projectionRead(t, f, "/pulls/2/runs", &history)
			if len(history.Items) != 1 {
				t.Fatal("fixture did not retain the external run")
			}
			run := history.Items[0]
			scope := f.scope(t, attemptProject, f.writer, false)
			at := stamp()
			roundHook(t, f, func(ctx context.Context, tx *sql.Tx) error {
				if err := reviews.FinalizeExternalHostedRun(ctx, tx, scope, reviews.NativeAdjudication{SourceRunID: run.SourceRunID, Finalized: true, FrozenCounts: &reviews.NativeCounts{}}); err != nil {
					return err
				}
				return creditExternalHostedRound(ctx, tx, attemptProject, 2, "fixture-batch", []string{run.ID}, 0, at)
			})
			roundHook(t, f, func(ctx context.Context, tx *sql.Tx) error {
				return creditExternalHostedRound(ctx, tx, attemptProject, 2, "fixture-batch", []string{run.ID}, 0, at)
			})
			err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				return creditExternalHostedRound(ctx, tx, attemptProject, 2, "duplicate-batch", []string{run.ID}, 0, at)
			})
			attemptCode(t, err, "review_round_external_conflict")
			f.configure(t, attemptProject, "test-capability", 2, false)
			request := f.enqueue(t, attemptProject, "later-hosted-native", "hosted", 2, 3)
			a := f.start(t, attemptProject, request)
			anchor := attemptAnchor(attemptProject, 2)
			anchor.ParentSHA = anchor.BaseSHA
			intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: anchor.CandidateSHA})
			if err != nil {
				t.Fatal(err)
			}
			a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, anchor, intent)
			if err != nil {
				t.Fatal(err)
			}
			a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "posted", "101")
			if err != nil {
				t.Fatal(err)
			}
			h, _, parsed := hostedEvidenceMetadataFixture()
			h.ObservedBaseSHA = anchor.ParentSHA
			h.Sources = []HostedFindingSource{}
			parsed.Findings, parsed.ReportedFindings = []reviewprovider.ParsedFinding{}, 0
			if alias {
				h.PublicationID = strings.TrimPrefix(run.SourceRunID, "publication:")
				h.ReviewIDs = []int64{401}
			}
			digest := strings.Repeat("1", 64)
			input := ReviewEvidenceInput{CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 3072, StartedAt: time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339Nano), FinishedAt: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), ExpectedAnchor: anchor, Review: parsed, Hosted: h}
			retained, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, input)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.e.PublishEvidence(t.Context(), attemptProject, retained.ID, nil); err != nil {
				t.Fatal(err)
			}
			if _, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "captured_native_result", ""); err != nil {
				t.Fatal(err)
			}
			roundFinalize(t, f, a, retained.ID, 0, false)
			got := roundRead(t, f, 2, "hosted")
			var ordinal int64
			err = f.store.DB().QueryRow(`SELECT ordinal FROM review_round_completions WHERE operation_id=?`, a.OperationID).Scan(&ordinal)
			if alias {
				if err != sql.ErrNoRows || got.Used != 1 || got.Dry != 1 || got.Tapered {
					t.Fatal("same result earned duplicate round", got, ordinal, err)
				}
			} else if err != nil || ordinal != 2 || got.Used != 2 || got.Dry != 2 || !got.Tapered {
				t.Fatal("native result did not follow the external ordinal", got, ordinal, err)
			}
			if err := ValidateRoundSnapshot(t.Context(), f.store.DB()); err != nil {
				t.Fatal("mixed history cannot be replayed", err)
			}
		})
	}
}

func TestExternalRoundCreditReleasesObsoleteQueuedCapacityWhilePaused(t *testing.T) {
	f, schedule, _ := lifecycleFixture(t)
	roundGrant(t, f, 2, "hosted", 0, 0, 1, "queued-round-budget")
	f.configure(t, attemptProject, "test-capability", 2, false)
	queued := f.enqueue(t, attemptProject, "queued-before-external-pickup", "hosted", 2, 3)
	f.configure(t, attemptProject, "test-capability", 3, true)
	lifecycleTick(t, f, schedule, externalReader(externalSnapshot(1, strings.Repeat("a", 40))))
	var history reviews.RunsResponse
	projectionRead(t, f, "/pulls/2/runs", &history)
	if len(history.Items) != 1 {
		t.Fatal("fixture did not retain the external run")
	}
	count := true
	request := ExternalHostedSettlement{CountRound: &count, Runs: []ExternalHostedSelectedRun{{RunID: history.Items[0].ID, Findings: []ExternalHostedDecision{}}}}
	_, err := f.e.SettleExternalHosted(t.Context(), f.scope(t, attemptProject, f.writer, false), "settle-while-paused", 2, request)
	if err != nil {
		t.Fatal("obsolete queued request blocked settlement", err)
	}
	projectionRead(t, f, "/pulls/2/runs", &history)
	op := roundOperation(t, f, queued.ID)
	if len(history.Items) != 1 || !history.Items[0].Finalized || roundRead(t, f, 2, "hosted").Used != 1 || op.State != "held" || op.Reason != "external_hosted_pending" {
		t.Fatal("settlement lost round atomicity or left a duplicate request runnable", op)
	}
	if err := ValidateRoundSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal("queued capacity result cannot be replayed", err)
	}
}
