package reviewruntime

import (
	"fmt"
	"strings"
	"testing"
)

func TestRecoverHostedReviewInfoIconKeepsOriginalAttemptAndFullFindings(t *testing.T) {
	f, captures, a, reader := recoveryRuntimeFixture(t, "uncertain")
	recoveryRuntimeMetadata(t, f, a)
	header := `<summary><picture><img src="https://assets.example.test/info.svg" width="16" height="16" align="absmiddle" alt=""></picture> Review info</summary>`
	review := &reader.record.Snapshot.Reviews[0]
	review.Body = strings.Replace(review.Body, "<summary>ℹ️ Review info</summary>", header, 1)
	review.Body = strings.Replace(review.Body, "comments posted: 1", "comments posted: 3", 1)
	for i := 2; i <= 3; i++ {
		root := reader.record.Snapshot.ReviewComments[0]
		root.ID += int64(i - 1)
		root.Body = hostedRetainInline(i, fmt.Sprintf("Preserve finding %d.", i))
		reader.record.Snapshot.ReviewComments = append(reader.record.Snapshot.ReviewComments, root)
	}
	beforeCredit := recoveryRuntimeCount(t, f, "review_round_completions")
	got, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || got.State != "completed" || got.EvidenceID == "" || got.TriggerCommentID != 100 {
		t.Fatal("decorated metadata prevented same-attempt recovery", got, err)
	}
	bound, err := f.engine.ReadHostedRecovery(t.Context(), f.project, a.ID, "runtime-capability")
	if err != nil || bound.Attempt.ID != a.ID || bound.Attempt.OperationID != a.OperationID || bound.Attempt.Anchor == nil || *bound.Attempt.Anchor != *a.Anchor || bound.Attempt.ExternalID != "100" || bound.Attempt.State != "completed" || bound.EvidenceID != got.EvidenceID {
		t.Fatal("recovery changed original identity", bound, err)
	}
	evidence, err := f.engine.ReadReviewEvidence(t.Context(), f.scopeWithAccess(t, f.reader, false, "read"), got.EvidenceID)
	if err != nil || evidence.Capture.Review.ReportedFindings != 3 || len(evidence.Capture.Review.Findings) != 3 || evidence.Capture.Hosted == nil || len(evidence.Capture.Hosted.Sources) != 3 {
		t.Fatal("recovery dropped findings or native source binding", evidence, err)
	}
	for i, finding := range evidence.Capture.Review.Findings {
		if finding.Detail != reader.record.Snapshot.ReviewComments[i].Body {
			t.Fatal("recovery changed original finding text")
		}
	}
	if recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f, "review_execution_reservations") != 0 || recoveryRuntimeCount(t, f, "review_round_completions") != beforeCredit {
		t.Fatal("recovery duplicated evidence, retained reservation or invented round credit")
	}
	calls := len(reader.calls)
	again, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || again.EvidenceID != got.EvidenceID || again.Reason != "already_completed" || len(reader.calls) != calls || recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 {
		t.Fatal("completed replay refetched or duplicated result", again, err)
	}
}
