package reviewruntime

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestRetainHostedPositiveComparisonBaselineReadFailureRetainsSourceAndRecovers(t *testing.T) {
	var prior reviewprovider.IssueComment
	f, record := runtimeStandingFixture(t, func(baseline *HostedCaptureRecord) {
		prior = reviewprovider.IssueComment{ID: 501, Body: "Previous synthetic provider report.", Author: baseline.ProviderActor, CreatedAt: baseline.StartedAt.Add(-time.Hour), UpdatedAt: baseline.StartedAt.Add(-time.Minute)}
		baseline.Snapshot.IssueComments = append(baseline.Snapshot.IssueComments, prior)
	}, nil)
	triggerAt := record.EarliestPosting
	// A complete positive publication lacks batch scope, so only the later
	// comparison fallback may load this baseline. This is not the zero-result or
	// incomplete-inventory fallback, which already reports baseline failures.
	record.Snapshot.IssueComments[1].CreatedAt = prior.CreatedAt
	record.Snapshot.IssueComments[1].UpdatedAt = triggerAt.Add(2 * time.Second)
	record.Snapshot.IssueComments[1].Body = strings.Replace(record.Snapshot.IssueComments[1].Body, "No actionable comments were generated in the recent review. 🎉\n", "", 1)
	record.Snapshot.IssueComments[1].Body = strings.Replace(record.Snapshot.IssueComments[1].Body, "pkg/source.go", "source.go", 1)
	record.Snapshot.IssueComments[2].CreatedAt = triggerAt.Add(3 * time.Second)
	record.Snapshot.IssueComments[2].UpdatedAt = triggerAt.Add(3 * time.Second)
	record.Snapshot.Reviews = []reviewprovider.HostedReview{{ID: 200, Body: hostedRetainPublicationMarker(), State: "commented", HeadSHA: f.f.headSHA, Author: record.ProviderActor, SubmittedAt: triggerAt.Add(time.Second)}}
	line := int64(7)
	record.Snapshot.ReviewComments = []reviewprovider.ReviewComment{{ID: 300, ReviewID: 200, Body: hostedRetainInline(1, "Preserve exact evidence."), Author: record.ProviderActor, Path: "source.go", Line: &line, OriginalLine: &line, Side: "RIGHT", HeadSHA: f.f.headSHA, OriginalHeadSHA: f.f.headSHA, CreatedAt: triggerAt.Add(time.Second), UpdatedAt: triggerAt.Add(time.Second)}}
	record.Snapshot.ObservedAt = triggerAt.Add(4 * time.Second)
	record.FinishedAt = record.Snapshot.ObservedAt
	inventory, err := reviewprovider.ExtractHostedFindings(record.Snapshot, record.Intent, record.PostingActor, record.ProviderActor, record.EarliestPosting)
	if err != nil || !inventory.Complete || inventory.Publication.State != "attributed" || inventory.Publication.StandingZero != nil || len(inventory.Publication.Roots) != 1 || len(inventory.Publication.Batches) != 1 {
		t.Fatal("fixture does not reach the positive comparison fallback", inventory, err)
	}
	if _, proven := reviewprovider.ResolveHostedComparison(inventory.Publication, f.f.headSHA, f.f.baseSHA, f.f.baseSHA); proven {
		t.Fatal("fixture unexpectedly has batch comparison proof")
	}
	journal, err := f.f.engine.ReadHostedObservations(t.Context(), f.f.project, f.a.ID)
	if err != nil || len(journal) != 1 {
		t.Fatal("missing original baseline", journal, err)
	}
	in := journal[0].Input
	baselineRef := CaptureReference{ID: in.CaptureID, SHA256: in.CaptureSHA256, Bytes: in.CaptureBytes}
	baselinePath := filepath.Join(f.captures.root, hostedCaptureFilename(f.a.ID, baselineRef))
	baselineRaw, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(baselinePath, []byte("corrupt test-only baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	_, wantErr := f.captures.Read(t.Context(), f.a.ID, baselineRef)
	if wantErr == nil {
		t.Fatal("baseline corruption did not produce the storage error")
	}
	before := cliAttemptState(t, f.f.store, f.a.ID)
	result, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	if !reflect.DeepEqual(err, wantErr) || result.Reason != "standing_baseline_unproven" || result.Complete || result.Receipt.ID != "" || !validHostedReference(result.Capture) {
		t.Fatalf("baseline error/result lost: result=%+v err=%v want=%v", result, err, wantErr)
	}
	retained, err := f.captures.Read(t.Context(), f.a.ID, result.Capture)
	if err != nil || !reflect.DeepEqual(retained, record) || cliAttemptState(t, f.f.store, f.a.ID) != before || cliCount(t, f.f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, f.a.ID) != 0 {
		t.Fatal("failed baseline lost raw source, changed the attempt or earned credit", err)
	}
	rawPath := filepath.Join(f.captures.root, hostedCaptureFilename(f.a.ID, result.Capture))
	rawBefore, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	// Repair only this test fixture's baseline, then reuse the original source.
	if err = os.WriteFile(baselinePath, baselineRaw, 0600); err != nil {
		t.Fatal(err)
	}
	recovered, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || !recovered.Complete || recovered.Capture != result.Capture || cliAttemptState(t, f.f.store, f.a.ID) != before || cliCount(t, f.f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, f.a.ID) != 1 {
		t.Fatal("original source did not recover once after baseline repair", recovered, err)
	}
	rawAfter, err := os.ReadFile(rawPath)
	if err != nil || string(rawAfter) != string(rawBefore) {
		t.Fatal("recovery changed the original raw observation", err)
	}
	journal, err = f.f.engine.ReadHostedObservations(t.Context(), f.f.project, f.a.ID)
	if err != nil || len(journal) != 2 || journal[0].Input.CaptureID != baselineRef.ID || journal[1].Input.CaptureID != result.Capture.ID {
		t.Fatal("recovery changed baseline/source identity or appended another capture", journal, err)
	}
}
