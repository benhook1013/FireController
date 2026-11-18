package reviewruntime

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRecoverHostedContinuationKeepsOriginalAttempt(t *testing.T) {
	f, captures, a, reader := recoveryRuntimeFixture(t, "uncertain")
	recoveryRuntimeMetadata(t, f, a)
	first := &reader.record.Snapshot.Reviews[0]
	first.Body = strings.Replace(first.Body, "batch=1/1", "batch=1/2", 1)
	second := *first
	second.ID++
	second.SubmittedAt = second.SubmittedAt.Add(time.Millisecond)
	second.Body = "<!-- coderabbit-ai-generated:v1 -->\n\n**Review continued from previous batch...**\n\n" + strings.Replace(hostedRetainPublicationMarker(), "batch=1/1", "batch=2/2", 1)
	reader.record.Snapshot.Reviews = append(reader.record.Snapshot.Reviews, second)
	got, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || got.State != "completed" || got.EvidenceID == "" || got.TriggerCommentID != 100 {
		t.Fatal("continuation did not recover", got, err)
	}
	evidence, err := f.engine.ReadReviewEvidence(t.Context(), f.scopeWithAccess(t, f.reader, false, "read"), got.EvidenceID)
	if err != nil || evidence.AttemptID != a.ID || evidence.OperationID != a.OperationID || evidence.Anchor != *a.Anchor || evidence.Capture.Review.ReportedFindings != 1 || evidence.Capture.Hosted == nil || !reflect.DeepEqual(evidence.Capture.Hosted.ReviewIDs, []int64{200, 201}) {
		t.Fatal("original binding or batch inventory changed", evidence, err)
	}
	if recoveryRuntimeCount(t, f, "review_execution_attempts") != 1 || recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f, "review_round_completions") != 0 {
		t.Fatal("recovery duplicated attempt or granted adjudication credit")
	}
	calls := len(reader.calls)
	again, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || again.Reason != "already_completed" || again.EvidenceID != got.EvidenceID || len(reader.calls) != calls {
		t.Fatal("replay refetched or duplicated", again, err)
	}
}
