package reviewruntime

import (
	"strings"
	"testing"
)

func TestRecoverHostedAdditionalCommentsSameAttemptOnceOnly(t *testing.T) {
	f, captures, a, reader := recoveryRuntimeFixture(t, "uncertain")
	recoveryRuntimeMetadata(t, f, a)
	item := "`24-24`: **Trivial** · **Maintainability & Code Quality** · **Quick win**\n\n⚠️ **Unverified finding**\nVerification did not confirm this synthetic observation.\n\n**Preserve the optional annotation.**\n\nIndependent rationale."
	category := "<details>\n<summary>🔇 Additional comments: 1</summary><blockquote>\n<dl>\n<dd>\n<details>\n<summary>source/optional.go-24-24: 1</summary><blockquote>\n<dl>\n<dd>\n" + item + "\n</dd>\n</dl>\n</blockquote></details>\n</dd>\n</dl>\n</blockquote></details>"
	reader.record.Snapshot.Reviews[0].Body += "\n\n" + category
	final, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || final.State != "completed" || final.EvidenceID == "" {
		t.Fatal("additional inventory failed original recovery", final, err)
	}
	evidence, err := f.engine.ReadReviewEvidence(t.Context(), f.scopeWithAccess(t, f.reader, false, "read"), final.EvidenceID)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.AttemptID != a.ID || evidence.OperationID != a.OperationID || evidence.Anchor != *a.Anchor || len(evidence.Capture.Review.Findings) != 2 || len(evidence.Capture.Hosted.Sources) != 2 {
		t.Fatal("lost original binding or additional finding")
	}
	finding := evidence.Capture.Review.Findings[1]
	source := evidence.Capture.Hosted.Sources[1]
	if finding.Detail != item || !strings.Contains(finding.Detail, "Unverified finding") || source.Kind != "additional" || source.CommentID != 0 || source.ReviewID != 200 {
		t.Fatal("source attribution or qualification changed")
	}
	transitions := recoveryRuntimeCount(t, f, "review_execution_transitions")
	calls := len(reader.calls)
	replay, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || replay.Reason != "already_completed" || replay.EvidenceID != final.EvidenceID || len(reader.calls) != calls || recoveryRuntimeCount(t, f, "review_execution_transitions") != transitions {
		t.Fatal("replay duplicated recovery", replay, err)
	}
	if recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f, "review_execution_reservations") != 0 || recoveryRuntimeCount(t, f, "review_round_completions") != 0 {
		t.Fatal("recovery duplicated evidence or awarded unadjudicated credit")
	}
}
