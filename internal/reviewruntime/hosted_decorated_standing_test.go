package reviewruntime

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// Use the complete pre-POST baseline owned by the existing runtime fixture.
// Decoration changes presentation; all comparison, run and comment identities
// remain the original provider facts.
func runtimeDecoratedStandingFixture(t *testing.T, dark bool) (*runtimeAllowanceFixture, HostedCaptureRecord) {
	t.Helper()
	f, record := runtimeAnnotatedStandingFixture(t, nil)
	source := ""
	if dark {
		source = `<source media="(prefers-color-scheme: dark)" srcset="https://assets.invalid/icon-dark.svg">`
	}
	icon := `<picture>` + source + `<img src="https://assets.invalid/icon.svg" width="16" height="16" align="absmiddle" alt=""></picture>`
	body := record.Snapshot.IssueComments[1].Body
	for _, label := range []string{"ℹ️ Recent review info", "⚙️ Run configuration", "📥 Commits", "⛔ Files ignored due to path filters:", "📒 Files selected for processing:"} {
		_, text, _ := strings.Cut(label, " ")
		body = strings.ReplaceAll(body, label, icon+" "+text)
	}
	body = strings.Replace(body, "🔵 Low", icon+" Minimal", 1)
	record.Snapshot.IssueComments[1].Body = strings.Replace(body, "No actionable comments were generated in the recent review. 🎉", "No actionable comments were generated in the recent review.", 1)
	ack := &record.Snapshot.IssueComments[2]
	ack.Body = "<!-- coderabbit-ai-generated:v1 -->\n\n" + strings.Replace(ack.Body, "Full review finished.", "<dl>\n<dd>\n\nFull review finished.\n\n</dd>\n</dl>", 1)
	root := &record.Snapshot.ReviewComments[0]
	root.Body = strings.Replace(root.Body, "✅ Addressed in commit "+record.Intent.HeadSHA[:7], "✅ Addressed in commits 0123456 to "+record.Intent.HeadSHA[:7], 1)
	return f, record
}

func TestRetainHostedDecoratedStandingOriginalAttemptAndFinalization(t *testing.T) {
	for _, dark := range []bool{false, true} {
		name := "img"
		if dark {
			name = "dark_source_and_img"
		}
		t.Run(name, func(t *testing.T) {
			f, record := runtimeDecoratedStandingFixture(t, dark)
			original := f.a
			before := cliAttemptState(t, f.f.store, original.ID)
			journal, err := f.f.engine.ReadHostedObservations(t.Context(), f.f.project, original.ID)
			if err != nil || len(journal) != 1 || journal[0].Input.Kind != "allowance_query" {
				t.Fatal("missing original full pre-POST baseline", err)
			}
			in := journal[0].Input
			baselineRef := CaptureReference{ID: in.CaptureID, SHA256: in.CaptureSHA256, Bytes: in.CaptureBytes}
			baselineRaw := hostedBridgeBytes(t, f, baselineRef)
			baseline, err := f.captures.Read(t.Context(), original.ID, baselineRef)
			if err != nil || len(baseline.Snapshot.Reviews) != 1 || len(baseline.Snapshot.ReviewComments) != 1 {
				t.Fatal("baseline lost original review/root", err)
			}
			root := record.Snapshot.ReviewComments[0]
			root.Body, root.UpdatedAt = baseline.Snapshot.ReviewComments[0].Body, baseline.Snapshot.ReviewComments[0].UpdatedAt
			if !reflect.DeepEqual(root, baseline.Snapshot.ReviewComments[0]) || !reflect.DeepEqual(record.Snapshot.Reviews, baseline.Snapshot.Reviews) {
				t.Fatal("fixture changed more than the original root footer/time")
			}
			inputBefore, _ := json.Marshal(record)
			retained, err := RetainHosted(t.Context(), f.captures, f.f.engine, original, record)
			if err != nil || !retained.Complete || retained.Reason != "capture_recorded" {
				t.Fatal("decorated standing result not retained", retained, err)
			}
			evidence, err := f.f.engine.ReadReviewEvidence(t.Context(), f.f.scopeWithAccess(t, f.f.reader, false, "read"), retained.Receipt.ID)
			if err != nil || evidence.AttemptID != original.ID || evidence.OperationID != original.OperationID || evidence.Anchor != *original.Anchor {
				t.Fatal("capture changed original attempt or anchor", err)
			}
			h := evidence.Capture.Hosted
			if h == nil || h.StandingSummary == nil || h.StandingSummary.CommentID != 501 || h.StandingSummary.ProviderRunID != runtimeStandingRun || h.TriggerCommentID != 100 || h.ObservedBaseSHA != original.Anchor.ParentSHA || h.PublicationID != "" || h.ProviderAttemptID != "" || len(h.ReviewIDs) != 0 || len(h.Sources) != 0 || evidence.Capture.Review.ReportedFindings != 0 || len(evidence.Capture.Review.Findings) != 0 || !evidence.Capture.Review.Complete {
				t.Fatal("lost exact standing-zero proof or invented review identity", evidence)
			}
			adjudication, err := f.f.engine.ReadReviewAdjudication(t.Context(), f.f.scope(t, f.f.writer, false), retained.Receipt.ID)
			if err != nil || adjudication.Finalized || adjudication.FrozenCounts != nil || recoveryRuntimeCount(t, f.f, "review_round_completions") != 0 || cliAttemptState(t, f.f.store, original.ID) != before {
				t.Fatal("capture granted completion credit or changed reservation/intent", err)
			}
			currentRaw := hostedBridgeBytes(t, f, retained.Capture)
			for range 2 {
				again, readErr := RetainHosted(t.Context(), f.captures, f.f.engine, original, record)
				if readErr != nil || again != retained || cliAttemptState(t, f.f.store, original.ID) != before {
					t.Fatal("repeated read changed original attempt", again, readErr)
				}
			}
			journal, err = f.f.engine.ReadHostedObservations(t.Context(), f.f.project, original.ID)
			if err != nil || len(journal) != 2 || journal[0].Input != in || recoveryRuntimeCount(t, f.f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f.f, "review_execution_attempts") != 1 || recoveryRuntimeCount(t, f.f, "review_operations") != 1 {
				t.Fatal("repeated capture duplicated evidence/operation or changed baseline", err)
			}
			inputAfter, _ := json.Marshal(record)
			if !bytes.Equal(inputBefore, inputAfter) || !bytes.Equal(baselineRaw, hostedBridgeBytes(t, f, baselineRef)) || !bytes.Equal(currentRaw, hostedBridgeBytes(t, f, retained.Capture)) {
				t.Fatal("retention changed raw source records")
			}
			zero := int64(0)
			if _, err = f.f.engine.FinalizeReviewEvidence(t.Context(), f.f.scope(t, f.f.writer, false), "decorated-premature", retained.Receipt.ID, reviewexec.FinalizeReviewRequest{ExpectedRevision: &zero}); err == nil {
				t.Fatal("active result finalized")
			}
			f.a, err = f.f.engine.Advance(t.Context(), f.f.project, original.ID, original.Revision, "completed", "synthetic_complete", original.ExternalID)
			if err != nil {
				t.Fatal(err)
			}
			adjudication, err = f.f.engine.ReadReviewAdjudication(t.Context(), f.f.scope(t, f.f.writer, false), retained.Receipt.ID)
			if err != nil || adjudication.Finalized || recoveryRuntimeCount(t, f.f, "review_round_completions") != 0 {
				t.Fatal("settlement alone finalized source or granted round credit", err)
			}
			if _, err = f.f.engine.FinalizeReviewEvidence(t.Context(), f.f.scope(t, f.f.writer, false), "decorated-finalize", retained.Receipt.ID, reviewexec.FinalizeReviewRequest{ExpectedRevision: &zero}); err != nil {
				t.Fatal(err)
			}
			adjudication, err = f.f.engine.ReadReviewAdjudication(t.Context(), f.f.scope(t, f.f.writer, false), retained.Receipt.ID)
			if err != nil || !adjudication.Finalized || adjudication.FrozenCounts == nil || *adjudication.FrozenCounts != (reviewexec.FindingCounts{}) || recoveryRuntimeCount(t, f.f, "review_execution_reservations") != 0 {
				t.Fatal("deliberate zero finalization failed", err)
			}
		})
	}
}

func TestRetainHostedDecoratedStandingBareAckHasNoInventoryOrCredit(t *testing.T) {
	f, record := runtimeDecoratedStandingFixture(t, true)
	// An attributable finished ACK is not a substitute for the current run's
	// comparison, selected files and explicit zero inventory.
	record.Snapshot.IssueComments = append(record.Snapshot.IssueComments[:1], record.Snapshot.IssueComments[2])
	inventory, err := reviewprovider.ExtractHostedFindings(record.Snapshot, record.Intent, record.PostingActor, record.ProviderActor, record.EarliestPosting)
	if err != nil || inventory.Complete || inventory.Publication.State != "uncertain" || inventory.Publication.Trigger.State != "adopted" || inventory.Reason != "hosted_finished_inventory_unproven" {
		t.Fatal("finished ACK without inventory did not remain uncertain", inventory, err)
	}
	before := cliAttemptState(t, f.f.store, f.a.ID)
	inputBefore, _ := json.Marshal(record)
	retained, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || retained.Complete || retained.Reason != inventory.Reason || !validHostedReference(retained.Capture) || retained.Receipt.ID != "" || recoveryRuntimeCount(t, f.f, "review_execution_evidence") != 0 || recoveryRuntimeCount(t, f.f, "review_round_completions") != 0 || cliAttemptState(t, f.f.store, f.a.ID) != before || recoveryRuntimeCount(t, f.f, "review_execution_reservations") != 1 {
		t.Fatal("ACK without inventory gained evidence/credit or lost original reservation", retained, err)
	}
	raw, err := f.captures.Read(t.Context(), f.a.ID, retained.Capture)
	inputAfter, _ := json.Marshal(record)
	if err != nil || !reflect.DeepEqual(raw, record) || !bytes.Equal(inputBefore, inputAfter) {
		t.Fatal("uncertain original capture not retained intact", err)
	}
}
