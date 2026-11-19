package reviewruntime

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRecoverHostedMinorGroupPreservesOriginalAttempt(t *testing.T) {
	f, captures, a, reader := recoveryRuntimeFixture(t, "uncertain")
	recoveryRuntimeMetadata(t, f, a)
	icon := `<picture><img src="https://assets.invalid/minor.svg" width="16" height="16" align="absmiddle" alt=""></picture>`
	outside := "<details>\n<summary>" + icon + " Minor comments: 1</summary><blockquote>\n<dl>\n<dd>\n<br>\n\n<details>\n<summary>engine/events.go: 1</summary><blockquote>\n<dl>\n<dd>\n<br>\n\n`19-19`: **" + icon + " Minor** · **Functional Correctness** · **Low value**\n\n**Preserve the minor finding.**\n\nKeep its original source text.\n\n<!-- cr-comment:v1:000000000000000000000002 -->\n\n</dd>\n</dl>\n</blockquote></details>\n\n</dd>\n</dl>\n</blockquote></details>\n\n"
	review := &reader.record.Snapshot.Reviews[0]
	review.Body = outside + review.Body
	original, _ := json.Marshal(reader.record)
	got, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || got.State != "completed" || got.EvidenceID == "" || got.TriggerCommentID != 100 {
		t.Fatal("original mixed finding inventory failed to recover", got, err)
	}
	bound, err := f.engine.ReadHostedRecovery(t.Context(), f.project, a.ID, "runtime-capability")
	if err != nil || bound.Attempt.ID != a.ID || bound.Attempt.OperationID != a.OperationID || bound.Attempt.ExternalID != "100" || bound.Attempt.Anchor == nil || *bound.Attempt.Anchor != *a.Anchor || bound.Attempt.Intent != a.Intent {
		t.Fatal("recovery replaced original issued identity", err)
	}
	evidence, err := f.engine.ReadReviewEvidence(t.Context(), f.scopeWithAccess(t, f.reader, false, "read"), got.EvidenceID)
	if err != nil || evidence.Capture.Review.ReportedFindings != 2 || len(evidence.Capture.Review.Findings) != 2 || evidence.Capture.Hosted == nil || len(evidence.Capture.Hosted.Sources) != 2 {
		t.Fatal("inline or outside finding lost", err)
	}
	seen := map[string]bool{}
	for i, source := range evidence.Capture.Hosted.Sources {
		finding := evidence.Capture.Review.Findings[i]
		seen[source.Kind] = true
		if source.Kind == "inline" && finding.Detail != reader.record.Snapshot.ReviewComments[0].Body {
			t.Fatal("inline source changed")
		}
		if source.Kind == "minor" && (!strings.Contains(review.Body, finding.Detail) || !strings.Contains(finding.Detail, icon) || finding.Title != "Preserve the minor finding.") {
			t.Fatal("outside source changed")
		}
	}
	if !seen["inline"] || !seen["minor"] {
		t.Fatal("mixed inventory lost source kinds")
	}
	adjudication, err := f.engine.ReadReviewAdjudication(t.Context(), f.scope(t, f.writer, false), got.EvidenceID)
	if err != nil || adjudication.Finalized || adjudication.FrozenCounts != nil || recoveryRuntimeCount(t, f, "review_round_completions") != 0 || recoveryRuntimeCount(t, f, "review_execution_reservations") != 0 {
		t.Fatal("capture invented completion credit/finalization or retained reservation", err)
	}
	calls := len(reader.calls)
	again, err := recoverRuntime(t, f, captures, a, reader)
	after, _ := json.Marshal(reader.record)
	if err != nil || again.Reason != "already_completed" || again.EvidenceID != got.EvidenceID || len(reader.calls) != calls || !reflect.DeepEqual(original, after) || recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f, "review_execution_attempts") != 1 || recoveryRuntimeCount(t, f, "review_operations") != 1 {
		t.Fatal("completed replay changed source or duplicated original attempt", err)
	}
}
