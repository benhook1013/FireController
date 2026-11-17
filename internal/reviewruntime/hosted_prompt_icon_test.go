package reviewruntime

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func runtimeSixFindingPrompt(t *testing.T, reader *hostedRecoveryReader, dark bool) {
	t.Helper()
	source := ""
	if dark {
		source = `<source media="(prefers-color-scheme: dark)" srcset="https://assets.invalid/prompt-dark.svg">`
	}
	header := `<summary><picture>` + source + `<img src="https://assets.invalid/prompt.svg" width="16" height="16" align="absmiddle" alt=""></picture> Prompt to fix review comments</summary>`
	prompt := "<details>\n" + header + "\n\n```\nSynthetic instructions only; these are not additional review findings.\n**Actionable comments posted: 999**\n```\n\n</details>\n\n"
	review := &reader.record.Snapshot.Reviews[0]
	review.Body = strings.Replace(review.Body, "comments posted: 1", "comments posted: 6", 1)
	review.Body = strings.Replace(review.Body, "<details>\n<summary>ℹ️ Review info</summary>", prompt+"<details>\n<summary>ℹ️ Review info</summary>", 1)
	root := reader.record.Snapshot.ReviewComments[0]
	reader.record.Snapshot.ReviewComments = reader.record.Snapshot.ReviewComments[:0]
	for i := 1; i <= 6; i++ {
		item := root
		item.ID += int64(i - 1)
		item.Path = fmt.Sprintf("engine/source%d.go", i)
		item.Body = hostedRetainInline(i, fmt.Sprintf("Preserve synthetic finding %d.", i))
		reader.record.Snapshot.ReviewComments = append(reader.record.Snapshot.ReviewComments, item)
	}
}

func TestRecoverHostedPromptIconPreservesSixFindingsAndOriginalAttempt(t *testing.T) {
	for _, dark := range []bool{false, true} {
		t.Run(fmt.Sprintf("dark_source_%t", dark), func(t *testing.T) {
			f, captures, a, reader := recoveryRuntimeFixture(t, "uncertain")
			recoveryRuntimeMetadata(t, f, a)
			runtimeSixFindingPrompt(t, reader, dark)
			originalSource, _ := json.Marshal(reader.record)
			got, err := recoverRuntime(t, f, captures, a, reader)
			if err != nil || got.State != "completed" || got.EvidenceID == "" || got.TriggerCommentID != 100 {
				t.Fatal("decorated prompt prevented six-finding recovery", got, err)
			}
			bound, err := f.engine.ReadHostedRecovery(t.Context(), f.project, a.ID, "runtime-capability")
			if err != nil || bound.Attempt.ID != a.ID || bound.Attempt.OperationID != a.OperationID || bound.Attempt.Anchor == nil || *bound.Attempt.Anchor != *a.Anchor || bound.Attempt.Intent != a.Intent || bound.Attempt.ExternalID != "100" || bound.Attempt.State != "completed" || bound.EvidenceID != got.EvidenceID {
				t.Fatal("recovery replaced original intent/identity", bound, err)
			}
			evidence, err := f.engine.ReadReviewEvidence(t.Context(), f.scopeWithAccess(t, f.reader, false, "read"), got.EvidenceID)
			if err != nil || evidence.AttemptID != a.ID || evidence.OperationID != a.OperationID || evidence.Anchor != *a.Anchor || evidence.Capture.Review.ReportedFindings != 6 || len(evidence.Capture.Review.Findings) != 6 || evidence.Capture.Hosted == nil {
				t.Fatal("six original findings or native binding lost", evidence, err)
			}
			h := evidence.Capture.Hosted
			if h.TriggerCommentID != 100 || h.ObservedBaseSHA != a.Anchor.ParentSHA || !reflect.DeepEqual(h.ReviewIDs, []int64{200}) || h.PublicationID != "11111111-1111-1111-1111-111111111111" || h.ProviderAttemptID != "22222222-2222-2222-2222-222222222222" || len(h.Sources) != 6 {
				t.Fatal("publication comparison/root inventory changed", h)
			}
			for i, source := range h.Sources {
				root := reader.record.Snapshot.ReviewComments[i]
				finding := evidence.Capture.Review.Findings[i]
				fingerprint := fmt.Sprintf("%024x", i+1)
				if source.Kind != "inline" || source.ReviewID != root.ReviewID || source.CommentID != root.ID || source.Fingerprint != fingerprint || source.SourceKey != fmt.Sprintf("hosted-comment:%d:%s", root.ID, fingerprint) || finding.Title != fmt.Sprintf("Preserve synthetic finding %d.", i+1) || finding.Detail != root.Body || finding.Path != root.Path {
					t.Fatal("recovery dropped/rewrote distinct original root", i)
				}
			}
			journal, err := f.engine.ReadHostedObservations(t.Context(), f.project, a.ID)
			if err != nil || len(journal) != 1 {
				t.Fatal("original observation missing/duplicated", err)
			}
			in := journal[0].Input
			ref := CaptureReference{ID: in.CaptureID, SHA256: in.CaptureSHA256, Bytes: in.CaptureBytes}
			raw, err := captures.Read(t.Context(), a.ID, ref)
			if err != nil || !reflect.DeepEqual(raw.Snapshot.Reviews, reader.record.Snapshot.Reviews) || !reflect.DeepEqual(raw.Snapshot.ReviewComments, reader.record.Snapshot.ReviewComments) || !reflect.DeepEqual(raw.Snapshot.IssueComments, reader.record.Snapshot.IssueComments) {
				t.Fatal("raw original provider facts changed", err)
			}
			adjudication, err := f.engine.ReadReviewAdjudication(t.Context(), f.scope(t, f.writer, false), got.EvidenceID)
			if err != nil || adjudication.Finalized || adjudication.FrozenCounts != nil || len(adjudication.Findings) != 6 || recoveryRuntimeCount(t, f, "review_round_completions") != 0 || recoveryRuntimeCount(t, f, "review_execution_reservations") != 0 || recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 {
				t.Fatal("capture invented finalized credit or retained reservation", err)
			}
			for _, decision := range adjudication.Findings {
				if decision.Disposition != "unadjudicated" {
					t.Fatal("recovery invented a finding decision")
				}
			}
			calls, transitions := len(reader.calls), recoveryRuntimeCount(t, f, "review_execution_transitions")
			again, err := recoverRuntime(t, f, captures, a, reader)
			afterSource, _ := json.Marshal(reader.record)
			if err != nil || again.Reason != "already_completed" || again.EvidenceID != got.EvidenceID || len(reader.calls) != calls || recoveryRuntimeCount(t, f, "review_execution_transitions") != transitions || recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f, "review_execution_attempts") != 1 || recoveryRuntimeCount(t, f, "review_operations") != 1 || string(originalSource) != string(afterSource) {
				t.Fatal("completed replay refetched/duplicated or changed original source", again, err)
			}
		})
	}
}

func TestRecoverHostedPromptUnknownFinishedInventoryStaysUncertainThenRecovers(t *testing.T) {
	f, captures, a, reader := recoveryRuntimeFixture(t, "uncertain")
	recoveryRuntimeMetadata(t, f, a)
	runtimeSixFindingPrompt(t, reader, true)
	supported := hostedRetainClone(t, reader.record)
	review := &reader.record.Snapshot.Reviews[0]
	start := strings.Index(review.Body, "<summary><picture>")
	if start < 0 {
		t.Fatal("decorated prompt fixture missing")
	}
	end := strings.Index(review.Body[start:], "</summary>") + start + len("</summary>")
	review.Body = review.Body[:start] + "<summary>Unknown review instructions</summary>" + review.Body[end:]
	inventory, err := reviewprovider.ExtractHostedFindings(reader.record.Snapshot, reader.record.Intent, reader.record.PostingActor, reader.record.ProviderActor, reader.record.EarliestPosting)
	if err != nil || inventory.Complete || inventory.Reason != "summary_format_unsupported" || inventory.Publication.State != "attributed" || inventory.Publication.FinishedAt.IsZero() {
		t.Fatal("fixture must isolate decoder refusal of an attributable finished publication", inventory, err)
	}
	got, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || got.State != "uncertain" || got.Reason != "hosted_finished_inventory_unproven" || got.EvidenceID != "" || got.TriggerCommentID != 100 {
		t.Fatal("finished attributed publication with unknown summary was not held", got, err)
	}
	bound, err := f.engine.ReadHostedRecovery(t.Context(), f.project, a.ID, "runtime-capability")
	if err != nil || bound.Attempt.State != "uncertain" || bound.Operation.Reason != got.Reason || bound.Attempt.ExternalID != "100" || *bound.Attempt.Anchor != *a.Anchor || bound.Attempt.Intent != a.Intent || recoveryRuntimeCount(t, f, "review_execution_reservations") != 1 || recoveryRuntimeCount(t, f, "review_execution_evidence") != 0 || recoveryRuntimeCount(t, f, "review_round_completions") != 0 {
		t.Fatal("unsupported summary lost original ownership or gained credit", err)
	}
	journal, err := f.engine.ReadHostedObservations(t.Context(), f.project, a.ID)
	if err != nil || len(journal) != 1 {
		t.Fatal("unknown complete raw source not journaled", err)
	}
	in := journal[0].Input
	ref := CaptureReference{ID: in.CaptureID, SHA256: in.CaptureSHA256, Bytes: in.CaptureBytes}
	raw, err := captures.Read(t.Context(), a.ID, ref)
	if err != nil || !reflect.DeepEqual(raw.Snapshot.Reviews, reader.record.Snapshot.Reviews) || !reflect.DeepEqual(raw.Snapshot.ReviewComments, reader.record.Snapshot.ReviewComments) || !reflect.DeepEqual(raw.Snapshot.IssueComments, reader.record.Snapshot.IssueComments) {
		t.Fatal("unsupported publication/root source lost", err)
	}
	transitions := recoveryRuntimeCount(t, f, "review_execution_transitions")
	again, err := recoverRuntime(t, f, captures, a, reader)
	repeated, readErr := f.engine.ReadHostedRecovery(t.Context(), f.project, a.ID, "runtime-capability")
	if err != nil || readErr != nil || again.State != "uncertain" || again.Reason != got.Reason || repeated.Attempt.Revision != bound.Attempt.Revision || repeated.Operation.Revision != bound.Operation.Revision || recoveryRuntimeCount(t, f, "review_execution_transitions") != transitions || recoveryRuntimeCount(t, f, "review_execution_reservations") != 1 || recoveryRuntimeCount(t, f, "review_execution_evidence") != 0 {
		t.Fatal("repeat finished unsupported source churned or gained evidence", again, err, readErr)
	}
	references, err := f.engine.ReadHostedObservations(t.Context(), f.project, a.ID)
	if err != nil || len(references) != 1 || references[0].Input != in {
		t.Fatal("identical unsupported source duplicated its observation", err)
	}
	reader.record = supported
	final, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || final.State != "completed" || final.EvidenceID == "" || final.TriggerCommentID != 100 || recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f, "review_execution_reservations") != 0 || recoveryRuntimeCount(t, f, "review_round_completions") != 0 {
		t.Fatal("supported inventory did not recover the original held attempt", final, err)
	}
	retained, err := captures.Read(t.Context(), a.ID, ref)
	if err != nil || !reflect.DeepEqual(retained, raw) {
		t.Fatal("successful recovery replaced earlier unsupported raw source", err)
	}
}
