package reviewruntime

import (
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
	"strings"
	"testing"
	"time"
)

func TestHostedStandingContentMarkerRecoveryAndBlockedDiagnostic(t *testing.T) {
	var old reviewprovider.IssueComment
	f, record := runtimeStandingFixture(t, func(b *HostedCaptureRecord) {
		old = reviewprovider.IssueComment{ID: 9001, Author: reviewprovider.Author{ID: 9002, Login: "synthetic-build[bot]", Kind: "Bot"}, CreatedAt: b.StartedAt.Add(-time.Hour), UpdatedAt: b.StartedAt.Add(-time.Minute), Body: "Earlier build report."}
		b.Snapshot.IssueComments = append(b.Snapshot.IssueComments, old)
	}, nil)
	if err := reviews.EnsureSchema(t.Context(), f.f.store); err != nil {
		t.Fatal(err)
	}
	var err error
	f.f.engine, err = reviewexec.New(f.f.store, []reviewexec.Capability{{ID: "runtime-capability", Provider: "coderabbit", Account: "runtime-account", Channels: []string{"cli", "hosted"}}}, reviewexec.WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	recoveryRuntimeMetadata(t, f.f, f.a)
	for i := range record.Snapshot.IssueComments {
		c := &record.Snapshot.IssueComments[i]
		c.UpdatedAt = c.UpdatedAt.Add(2 * time.Second)
		if i != 1 {
			c.CreatedAt = c.CreatedAt.Add(2 * time.Second)
		}
	}
	record.Snapshot.ObservedAt = record.Snapshot.ObservedAt.Add(2 * time.Second)
	record.FinishedAt = record.Snapshot.ObservedAt
	current := old
	current.Body = "Fresh build report."
	current.UpdatedAt = record.Snapshot.IssueComments[0].CreatedAt.Add(time.Second)
	record.Snapshot.IssueComments = append(record.Snapshot.IssueComments, current)
	good := record.Snapshot.IssueComments[1].Body
	record.Snapshot.IssueComments[1].Body = "Unsupported retained outcome."
	reader := &hostedRecoveryReader{record: record, repositoryID: record.RepositoryID}
	first, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, reader.record)
	if err != nil || first.Complete || first.Reason != "hosted_finished_inventory_unproven" || first.Receipt.ID != "" {
		t.Fatal("finished capture refusal was hidden as provider wait", first, err)
	}
	// The driver maps this refusal to the existing durable uncertainty state.
	// Retain explicit fixture times instead of reading future fixture facts as
	// if the real wall-clock had already observed them.
	f.a, err = f.f.engine.Advance(t.Context(), f.f.project, f.a.ID, f.a.Revision, "uncertain", first.Reason, f.a.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	status, err := f.f.engine.ReadOperationStatus(t.Context(), f.f.scopeWithAccess(t, f.f.reader, false, "read"), f.a.OperationID)
	if err != nil || status.Stage != "capture_blocked" || status.NextAction != "contact_controller_owner" {
		t.Fatal("ordinary operation read hides capture refusal", status, err)
	}
	marker := "<summary><strong>ℹ️ Recent review info</strong></summary>\n<dl>\n<dd>\n"
	reader.record.Snapshot.IssueComments[1].Body = strings.Replace(good, marker, marker+"<!-- review_info_content -->\n", 1)
	if reader.record.Snapshot.IssueComments[1].Body == good {
		t.Fatal("marker fixture not applied")
	}
	retained, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, reader.record)
	if err != nil || !retained.Complete {
		t.Fatal("supported marker did not retain original result", retained, err)
	}
	final, err := recoverRuntime(t, f.f, f.captures, f.a, reader)
	if err != nil || final.State != "completed" || final.EvidenceID == "" {
		t.Fatal("original attempt did not recover", final, err)
	}
	e, err := f.f.engine.ReadReviewEvidence(t.Context(), f.f.scopeWithAccess(t, f.f.reader, false, "read"), final.EvidenceID)
	if err != nil || e.AttemptID != f.a.ID || e.OperationID != f.a.OperationID || e.Anchor != *f.a.Anchor || e.Capture.Review.ReportedFindings != 0 || e.Capture.Hosted.StandingSummary == nil {
		t.Fatal("standing result lost original binding", err)
	}
	transitions := recoveryRuntimeCount(t, f.f, "review_execution_transitions")
	calls := len(reader.calls)
	replay, err := recoverRuntime(t, f.f, f.captures, f.a, reader)
	if err != nil || replay.Reason != "already_completed" || replay.EvidenceID != final.EvidenceID || len(reader.calls) != calls || recoveryRuntimeCount(t, f.f, "review_execution_transitions") != transitions {
		t.Fatal("recovery replay duplicated work", replay, err)
	}
	if recoveryRuntimeCount(t, f.f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f.f, "review_execution_reservations") != 0 || recoveryRuntimeCount(t, f.f, "review_round_completions") != 0 {
		t.Fatal("recovery duplicated evidence or awarded credit before finalization")
	}
}
