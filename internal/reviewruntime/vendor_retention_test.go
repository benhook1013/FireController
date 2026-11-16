package reviewruntime

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVendorRetentionRequiresPreservedCompletedOldEvidence(t *testing.T) {
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	id := "review_attempt_11111111111111111111111111111111"
	old := now.Add(-8 * 24 * time.Hour).Format(time.RFC3339Nano)
	m := vendorSessionMetadata{Head: "candidate", BaseCommitID: "base", ComparisonBaseCommitID: "base"}
	ref := CaptureReference{ID: "capture", SHA256: "hash", Bytes: 123}
	e := reviewexec.ReviewEvidence{AttemptID: id, RecordedAt: old, Anchor: reviewexec.Anchor{CandidateSHA: "candidate", MergeBaseSHA: "base"}}
	e.Capture = reviewexec.ReviewEvidenceInput{CaptureID: ref.ID, CaptureSHA256: ref.SHA256, CaptureBytes: ref.Bytes, StartedAt: old, FinishedAt: old, ExpectedAnchor: e.Anchor}
	e.Capture.Review.Complete = true
	e.Capture.Review.Outcome = "completed"
	if !vendorRetentionEvidence(m, id, e, ref, now) {
		t.Fatal("qualified completed session rejected")
	}
	tests := map[string]func(*vendorSessionMetadata, *reviewexec.ReviewEvidence, *CaptureReference){
		"recent": func(_ *vendorSessionMetadata, e *reviewexec.ReviewEvidence, _ *CaptureReference) {
			e.RecordedAt = now.Add(-time.Hour).Format(time.RFC3339Nano)
		},
		"future": func(_ *vendorSessionMetadata, e *reviewexec.ReviewEvidence, _ *CaptureReference) {
			e.RecordedAt = now.Add(time.Hour).Format(time.RFC3339Nano)
		},
		"clock reversal": func(_ *vendorSessionMetadata, e *reviewexec.ReviewEvidence, _ *CaptureReference) {
			e.Capture.StartedAt = now.Format(time.RFC3339Nano)
		},
		"bad time": func(_ *vendorSessionMetadata, e *reviewexec.ReviewEvidence, _ *CaptureReference) {
			e.Capture.FinishedAt = "bad"
		},
		"failed": func(_ *vendorSessionMetadata, e *reviewexec.ReviewEvidence, _ *CaptureReference) {
			e.Capture.Review.Outcome = "failed"
		},
		"incomplete": func(_ *vendorSessionMetadata, e *reviewexec.ReviewEvidence, _ *CaptureReference) {
			e.Capture.Review.Complete = false
		},
		"wrong attempt": func(_ *vendorSessionMetadata, e *reviewexec.ReviewEvidence, _ *CaptureReference) {
			e.AttemptID = "other"
		},
		"wrong hash": func(_ *vendorSessionMetadata, _ *reviewexec.ReviewEvidence, r *CaptureReference) { r.SHA256 = "other" },
		"wrong size": func(_ *vendorSessionMetadata, _ *reviewexec.ReviewEvidence, r *CaptureReference) { r.Bytes++ },
		"wrong id":   func(_ *vendorSessionMetadata, _ *reviewexec.ReviewEvidence, r *CaptureReference) { r.ID = "other" },
		"wrong head": func(m *vendorSessionMetadata, _ *reviewexec.ReviewEvidence, _ *CaptureReference) { m.Head = "other" },
		"wrong base": func(m *vendorSessionMetadata, _ *reviewexec.ReviewEvidence, _ *CaptureReference) {
			m.BaseCommitID = "other"
		},
		"wrong comparison": func(m *vendorSessionMetadata, _ *reviewexec.ReviewEvidence, _ *CaptureReference) {
			m.ComparisonBaseCommitID = "other"
		},
		"wrong anchor": func(_ *vendorSessionMetadata, e *reviewexec.ReviewEvidence, _ *CaptureReference) {
			e.Capture.ExpectedAnchor.CandidateSHA = "other"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			mm, ee, rr := m, e, ref
			mutate(&mm, &ee, &rr)
			if vendorRetentionEvidence(mm, id, ee, rr, now) {
				t.Fatal("unsafe session qualified")
			}
		})
	}
}

func TestVendorRetentionRuntimeUsesJournalAndDurableCapture(t *testing.T) {
	f := newCLIDriverFixture(t)
	op, a := f.prepareAdmit(t, 2, 1, "vendor-retention")
	result, err := f.driver.Dispatch(t.Context(), op, a, nil)
	if err != nil || result.State != "completed" {
		t.Fatal(result, err)
	}
	meta, _ := json.Marshal(vendorSessionMetadata{Head: a.Anchor.CandidateSHA, BaseBranch: "refs/firecontroller/review-base/" + a.ID, BaseCommitID: a.Anchor.MergeBaseSHA, ComparisonBaseCommitID: a.Anchor.MergeBaseSHA, WorkingDirectory: "/data/workspaces/" + a.ID})
	marker := []byte(`{"version":2,"state":"complete","hasGitMetadata":true,"hasIncrementalDiff":true}`)
	future := time.Now().Add(8 * 24 * time.Hour)
	if ok, err := f.driver.qualifyVendorSession(t.Context(), meta, marker, future); err != nil || ok {
		t.Fatal("active/reserved session qualified", ok, err)
	}
	if _, err = f.engine.Advance(t.Context(), f.project, a.ID, a.Revision, result.State, result.Reason, ""); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.driver.qualifyVendorSession(t.Context(), meta, marker, time.Now()); err != nil || ok {
		t.Fatal("recent session qualified", ok, err)
	}
	if ok, err := f.driver.qualifyVendorSession(t.Context(), meta, marker, future); err != nil || !ok {
		t.Fatal("retained complete session not qualified", ok, err)
	}
	for _, bad := range [][]byte{nil, []byte(`{"version":1,"state":"complete"}`), []byte(`{"version":2,"state":"active","hasGitMetadata":true,"hasIncrementalDiff":true}`)} {
		if ok, err := f.driver.qualifyVendorSession(t.Context(), meta, bad, future); err != nil || ok {
			t.Fatal("invalid marker qualified", ok, err)
		}
	}

	var original string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_evidence WHERE attempt_id=?`, a.ID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"id", "project_id", "operation_id", "capability_fingerprint"} {
		var altered map[string]any
		if err := json.Unmarshal([]byte(original), &altered); err != nil {
			t.Fatal(err)
		}
		altered[field] = "different"
		encoded, _ := json.Marshal(altered)
		if _, err := f.store.DB().Exec(`UPDATE review_execution_evidence SET body_json=? WHERE attempt_id=?`, string(encoded), a.ID); err != nil {
			t.Fatal(err)
		}
		if ok, err := f.driver.qualifyVendorSession(t.Context(), meta, marker, future); err != nil || ok {
			t.Fatal("mismatched ledger body qualified", field, ok, err)
		}
	}
	if _, err := f.store.DB().Exec(`UPDATE review_execution_evidence SET body_json=? WHERE attempt_id=?`, original, a.ID); err != nil {
		t.Fatal(err)
	}
	// Damage only synthetic retained data; no new review or provider call.
	if err := os.WriteFile(filepath.Join(f.captures.root, a.ID+".json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.driver.qualifyVendorSession(t.Context(), meta, marker, future); err == nil || ok {
		t.Fatal("missing usable capture qualified", ok, err)
	}
}
