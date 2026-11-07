package reviewruntime

import (
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestRecoverHostedAdditionalCommentsSameAttemptOnceOnly(t *testing.T) {
	testRecoverHostedAdditionalComments(t, false)
}

func TestRecoverHostedAdditionalOnlyPresentationSameAttemptOnceOnly(t *testing.T) {
	testRecoverHostedAdditionalComments(t, true)
}

func testRecoverHostedAdditionalComments(t *testing.T, summaryOnly bool) {
	t.Helper()
	f, captures, a, reader := recoveryRuntimeFixture(t, "active")
	recoveryRuntimeMetadata(t, f, a)
	item := "`24-24`: **Trivial** · **Maintainability & Code Quality** · **Quick win**\n\n⚠️ **Unverified finding**\nVerification did not confirm this synthetic observation.\n\n**Preserve the optional annotation.**\n\nIndependent rationale."
	category := "<details>\n<summary>🔇 Additional comments: 1</summary><blockquote>\n<dl>\n<dd>\n<details>\n<summary>source/optional.go-24-24: 1</summary><blockquote>\n<dl>\n<dd>\n" + item + "\n</dd>\n</dl>\n</blockquote></details>\n</dd>\n</dl>\n</blockquote></details>"
	icon := `<picture><source media="(prefers-color-scheme: dark)" srcset="https://assets.example.test/dark.svg"><img src="https://assets.example.test/icon.svg" width="16" height="16" align="absmiddle" alt=""></picture>`
	category = strings.Replace(category, "🔇", icon, 1)
	info := "<details>\n<summary>" + icon + " Review info</summary>\n<dl>\n<dd>\nSynthetic provider metadata.\n</dd>\n</dl>\n</details>"
	wantFindings, findingIndex := 2, 1
	if summaryOnly {
		category = strings.ReplaceAll(category, "<dd>\n", "<dd>\n<br>\n")
		info = "<h6>\n" + info + "\n</h6>"
		reader.record.Snapshot.Reviews[0].Body = strings.Replace(reader.record.Snapshot.Reviews[0].Body, "**Actionable comments posted: 1**\n", "", 1)
		reader.record.Snapshot.ReviewComments = []reviewprovider.ReviewComment{}
		wantFindings, findingIndex = 1, 0
	}
	reader.record.Snapshot.Reviews[0].Body += "\n\n" + category + "\n\n" + info
	a, err := f.engine.Advance(t.Context(), a.ProjectID, a.ID, a.Revision, "uncertain", "hosted_finished_inventory_unproven", a.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != "uncertain" {
		t.Fatal("original blocked attempt lost")
	}
	final, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || final.State != "completed" || final.EvidenceID == "" {
		t.Fatal("additional inventory failed original recovery", final, err)
	}
	evidence, err := f.engine.ReadReviewEvidence(t.Context(), f.scopeWithAccess(t, f.reader, false, "read"), final.EvidenceID)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.AttemptID != a.ID || evidence.OperationID != a.OperationID || evidence.Anchor != *a.Anchor || len(evidence.Capture.Review.Findings) != wantFindings || len(evidence.Capture.Hosted.Sources) != wantFindings {
		t.Fatal("lost original binding or additional finding")
	}
	finding := evidence.Capture.Review.Findings[findingIndex]
	source := evidence.Capture.Hosted.Sources[findingIndex]
	if finding.Detail != item || !strings.Contains(finding.Detail, "Unverified finding") || source.Kind != "additional" || source.CommentID != 0 || source.ReviewID != 200 {
		t.Fatal("source attribution or qualification changed")
	}
	transitions := recoveryRuntimeCount(t, f, "review_execution_transitions")
	calls := len(reader.calls)
	replay, err := recoverRuntime(t, f, captures, a, reader)
	if err != nil || replay.Reason != "already_completed" || replay.EvidenceID != final.EvidenceID || len(reader.calls) != calls || recoveryRuntimeCount(t, f, "review_execution_transitions") != transitions {
		t.Fatal("replay duplicated recovery", replay, err)
	}
	if recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f, "review_execution_reservations") != 0 || nativeHostedCredit(t, f) != 0 {
		t.Fatal("recovery duplicated evidence or awarded unadjudicated credit")
	}
}
