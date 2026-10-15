package reviewruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func hostedBridgeState(t *testing.T, f *runtimeAllowanceFixture) string {
	t.Helper()
	// Include the actual attempt/operation, reservations, retained source journals,
	// events and receipts so a rejected bridge call cannot leave a partial effect.
	var state, intent, anchor, external, operation, query, preflight string
	var revision, operationRevision, events, receipts, reservations, transitions, evidence, observations int64
	db := f.f.store.DB()
	if err := db.QueryRow(`SELECT a.state,a.revision,a.intent,a.anchor_json,a.external_id,o.state,o.revision FROM review_execution_attempts a JOIN review_operations o ON o.id=a.operation_id WHERE a.id=?`, f.a.ID).Scan(&state, &revision, &intent, &anchor, &external, &operation, &operationRevision); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT body_json FROM review_hosted_allowance_queries WHERE attempt_id=?`, f.a.ID).Scan(&query); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT coalesce(group_concat(body_json,''),'') FROM review_hosted_preflight WHERE attempt_id=?`, f.a.ID).Scan(&preflight); err != nil {
		t.Fatal(err)
	}
	for table, target := range map[string]*int64{"events": &events, "receipts": &receipts, "review_execution_reservations": &reservations, "review_execution_transitions": &transitions, "review_execution_evidence": &evidence, "review_hosted_observations": &observations} {
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	encoded, err := json.Marshal([]any{state, revision, intent, anchor, external, operation, operationRevision, query, preflight, events, receipts, reservations, transitions, evidence, observations})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func hostedBridgeBytes(t *testing.T, f *runtimeAllowanceFixture, ref CaptureReference) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.captures.root, hostedCaptureFilename(f.a.ID, ref)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func hostedBridgeObserved(t *testing.T) (*runtimeAllowanceFixture, ObservedHostedAllowance, HostedCaptureRecord) {
	t.Helper()
	f := newRuntimeAllowanceFixture(t)
	got, err := f.observe(t)
	if err != nil {
		t.Fatal(err)
	}
	f.assertAvailableSource(t, got)
	source, err := f.captures.Read(t.Context(), f.a.ID, got.Capture)
	if err != nil {
		t.Fatal(err)
	}
	return f, got, source
}

func TestRetainHostedAllowanceActualCaptureThroughFinalActionGuard(t *testing.T) {
	f, got, source := hostedBridgeObserved(t)
	originalBytes := hostedBridgeBytes(t, f, got.Capture)
	originalQuery, err := f.f.engine.ReadHostedAllowanceQuery(t.Context(), f.f.project, f.a.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Parsed caller output is not an admission input: derive availability again
	// from the immutable reference even when that output was modified or lost.
	got.Observation = reviewprovider.HostedAllowanceObservation{State: "caller changed", Reason: "untrusted parsed result"}
	got.QueryID = "caller-changed"
	got.Reason = "caller-changed"
	beforeAttempt := hostedBridgeState(t, f)
	if err := RetainHostedAllowance(t.Context(), f.captures, f.f.engine, f.a, f.anchor, 2, got.Capture); err != nil {
		t.Fatal(err)
	}
	f.assertNotAdmitted(t)
	retained, err := f.f.engine.ReadHostedAllowance(t.Context(), f.f.project, f.a.ID)
	if err != nil || retained.Anchor != f.anchor || retained.ChangedFiles != 2 || retained.CaptureID != got.Capture.ID || retained.CaptureSHA256 != got.Capture.SHA256 || retained.CaptureBytes != got.Capture.Bytes || retained.QueryID != originalQuery.ID || retained.Observation.State != "available_observed" || retained.Observation.QueryCommentID != 100 || retained.Observation.ResponseCommentID != 101 || retained.ObservedAt != source.Snapshot.ObservedAt.Format(time.RFC3339Nano) {
		t.Fatal("bridge did not retain exact independently parsed source", retained, err)
	}
	after := hostedBridgeState(t, f)
	if beforeAttempt == after {
		t.Fatal("bridge did not record new preflight")
	}
	if err := RetainHostedAllowance(t.Context(), f.captures, f.f.engine, f.a, f.anchor, 2, got.Capture); err != nil || hostedBridgeState(t, f) != after {
		t.Fatal("same reference replay changed durable state", err)
	}
	body, err := reviewprovider.HostedTriggerBody(source.Intent)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := f.f.engine.Admit(t.Context(), f.f.project, f.a.ID, 1, f.anchor, body)
	if err != nil {
		t.Fatal(err)
	}
	after = hostedBridgeState(t, f)
	if err := f.f.engine.VerifyHostedAction(t.Context(), f.f.project, f.a.ID, admitted.Revision); err != nil || hostedBridgeState(t, f) != after {
		t.Fatal("retained source did not satisfy read-only final action guard", err)
	}
	if err := RetainHostedAllowance(t.Context(), f.captures, f.f.engine, f.a, f.anchor, 2, got.Capture); err != nil || hostedBridgeState(t, f) != after {
		t.Fatal("retained preflight replay after admission changed state", err)
	}
	if f.provider.posts != 1 || f.provider.reads != 1 || !bytes.Equal(originalBytes, hostedBridgeBytes(t, f, got.Capture)) {
		t.Fatal("bridge performed provider work or changed original source")
	}
	query, err := f.f.engine.ReadHostedAllowanceQuery(t.Context(), f.f.project, f.a.ID)
	if err != nil || !reflect.DeepEqual(query, originalQuery) {
		t.Fatal("bridge rewrote query evidence", err)
	}
	var count int
	if err := f.f.store.DB().QueryRow(`SELECT count(*) FROM events WHERE event_type='review.hosted_preflight'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("preflight replay repeated events", count, err)
	}
	if admitted.State != "posting" || admitted.ExternalID != "" || admitted.Intent != body {
		t.Fatal("test guard executed or fabricated provider action", admitted)
	}
}

func TestRetainHostedAllowanceRejectsMismatchedAndUnprovenSources(t *testing.T) {
	for name, change := range map[string]func(*HostedCaptureRecord){
		"repository identity": func(r *HostedCaptureRecord) { r.RepositoryID++ },
		"repository spelling": func(r *HostedCaptureRecord) { r.Repository = "foreign/repo" },
		"pull":                func(r *HostedCaptureRecord) { r.PullNumber++ },
		"operation":           func(r *HostedCaptureRecord) { r.Intent.OperationID = "review_operation_" + strings.Repeat("f", 32) },
		"head":                func(r *HostedCaptureRecord) { r.Intent.HeadSHA = strings.Repeat("f", 40) },
		"posting actor":       func(r *HostedCaptureRecord) { r.PostingActor.ID++ },
		"provider actor":      func(r *HostedCaptureRecord) { r.ProviderActor.ID++ },
		"posting window":      func(r *HostedCaptureRecord) { r.EarliestPosting = r.EarliestPosting.Add(-time.Second) },
		"query identity": func(r *HostedCaptureRecord) {
			r.Snapshot.IssueComments[0].ID++ /* distinct from reply */
			r.Snapshot.IssueComments[0].ID = 200
		},
		"unknown reply": func(r *HostedCaptureRecord) { r.Snapshot.IssueComments[1].Body = "Unrecognized provider reply" },
		"cooldown": func(r *HostedCaptureRecord) {
			r.Snapshot.IssueComments[1].Body = strings.Replace(runtimeAllowanceAvailable, "Reviews are available now.", "More reviews will be available in 37 minutes.", 1)
		},
		"ambiguous reply": func(r *HostedCaptureRecord) {
			c := r.Snapshot.IssueComments[1]
			c.ID++
			r.Snapshot.IssueComments = append(r.Snapshot.IssueComments, c)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, got, source := hostedBridgeObserved(t)
			raw := hostedBridgeBytes(t, f, got.Capture)
			change(&source)
			ref, err := f.captures.Save(t.Context(), f.a.ID, source)
			if err != nil {
				t.Fatal("malformed controlled fixture", err)
			}
			changedBytes := hostedBridgeBytes(t, f, ref)
			// Even an application object claiming success cannot replace the captured
			// negative facts; RetainHostedAllowance only accepts its reference.
			forged := ObservedHostedAllowance{Capture: ref, Observation: reviewprovider.HostedAllowanceObservation{State: "available_observed", QueryCommentID: 100, ResponseCommentID: 101}}
			before := hostedBridgeState(t, f)
			err = RetainHostedAllowance(t.Context(), f.captures, f.f.engine, f.a, f.anchor, 2, forged.Capture)
			assertCoreErrorCode(t, err, "review_held")
			if hostedBridgeState(t, f) != before || f.provider.posts != 1 || f.provider.reads != 1 || !bytes.Equal(raw, hostedBridgeBytes(t, f, got.Capture)) || !bytes.Equal(changedBytes, hostedBridgeBytes(t, f, ref)) {
				t.Fatal("rejected source changed journal/raw bytes or contacted provider")
			}
			f.assertNotAdmitted(t)
		})
	}
}

func TestRetainHostedAllowanceRejectsReferenceTampering(t *testing.T) {
	for _, kind := range []string{"id", "checksum", "bytes", "corrupt bytes"} {
		t.Run(kind, func(t *testing.T) {
			f, got, _ := hostedBridgeObserved(t)
			ref := got.Capture
			switch kind {
			case "id":
				ref.ID += "x"
			case "checksum":
				ref.SHA256 = strings.Repeat("f", 64)
				ref.ID = "review_capture_" + ref.SHA256[:32]
			case "bytes":
				ref.Bytes++
			case "corrupt bytes":
				path := filepath.Join(f.captures.root, hostedCaptureFilename(f.a.ID, ref))
				raw := hostedBridgeBytes(t, f, ref)
				if err := os.WriteFile(path, append(raw, ' '), 0600); err != nil {
					t.Fatal(err)
				}
			}
			raw := hostedBridgeBytes(t, f, got.Capture)
			before := hostedBridgeState(t, f)
			if err := RetainHostedAllowance(t.Context(), f.captures, f.f.engine, f.a, f.anchor, 2, ref); err == nil || hostedBridgeState(t, f) != before || !bytes.Equal(raw, hostedBridgeBytes(t, f, got.Capture)) || f.provider.posts != 1 || f.provider.reads != 1 {
				t.Fatal("invalid reference admitted, wrote state/source, or performed provider work", err)
			}
		})
	}
}

func TestRetainHostedAllowanceCurrentEligibilityStillRequired(t *testing.T) {
	for name, change := range map[string]func(*testing.T, *runtimeAllowanceFixture, *reviewexec.Anchor){
		"source changed": func(_ *testing.T, _ *runtimeAllowanceFixture, a *reviewexec.Anchor) {
			a.ParentSHA = strings.Repeat("f", 40)
		},
		"stale source": func(_ *testing.T, _ *runtimeAllowanceFixture, a *reviewexec.Anchor) {
			a.ObservedAt = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
		},
		"cancelled preparation": func(t *testing.T, f *runtimeAllowanceFixture, _ *reviewexec.Anchor) {
			if _, err := f.f.engine.Advance(t.Context(), f.f.project, f.a.ID, 1, "cancelled", "synthetic_cancelled", ""); err != nil {
				t.Fatal(err)
			}
		},
		"file ceiling": func(t *testing.T, f *runtimeAllowanceFixture, _ *reviewexec.Anchor) {
			if _, err := f.f.store.DB().Exec(`UPDATE review_policy_config SET body_json='{"required_channels":["cli","hosted"],"paused_channels":[],"max_review_files":1}' WHERE project_id=? AND revision=1`, f.f.project); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, got, _ := hostedBridgeObserved(t)
			anchor := f.anchor
			change(t, f, &anchor)
			before := hostedBridgeState(t, f)
			raw := hostedBridgeBytes(t, f, got.Capture)
			if err := RetainHostedAllowance(t.Context(), f.captures, f.f.engine, f.a, anchor, 2, got.Capture); err == nil || hostedBridgeState(t, f) != before || !bytes.Equal(raw, hostedBridgeBytes(t, f, got.Capture)) || f.provider.posts != 1 || f.provider.reads != 1 {
				t.Fatal("ineligible preflight succeeded or changed state", err)
			}
		})
	}
}

func TestRetainHostedAllowanceFreshFetchCannotRefreshOldProviderResponse(t *testing.T) {
	f, got, source := hostedBridgeObserved(t)
	query, err := f.f.engine.ReadHostedAllowanceQuery(t.Context(), f.f.project, f.a.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct an honestly historical query/response in this owned fixture.
	// Fetch/source observation remains current, but the actual provider response
	// is two minutes old and therefore cannot establish fresh availability.
	shift := -2 * time.Minute
	earliest, err := time.Parse(time.RFC3339Nano, query.EarliestPosting)
	if err != nil {
		t.Fatal(err)
	}
	query.EarliestPosting = earliest.Add(shift).Format(time.RFC3339Nano)
	query.Anchor.ObservedAt = earliest.Add(shift).Format(time.RFC3339Nano)
	query.Comment.CreatedAt = query.Comment.CreatedAt.Add(shift)
	query.Comment.UpdatedAt = query.Comment.UpdatedAt.Add(shift)
	body, err := json.Marshal(query)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.f.store.DB().Exec(`UPDATE review_hosted_allowance_queries SET body_json=? WHERE attempt_id=?`, string(body), f.a.ID); err != nil {
		t.Fatal(err)
	}
	oldCreated := earliest.Add(shift - time.Second).Format(time.RFC3339Nano)
	if _, err = f.f.store.DB().Exec(`UPDATE review_operations SET created_at=?,updated_at=? WHERE id=?`, oldCreated, oldCreated, f.a.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.f.store.DB().Exec(`UPDATE review_execution_transitions SET created_at=? WHERE attempt_id=?`, oldCreated, f.a.ID); err != nil {
		t.Fatal(err)
	}
	source.EarliestPosting = source.EarliestPosting.Add(shift)
	for i := range source.Snapshot.IssueComments {
		source.Snapshot.IssueComments[i].CreatedAt = source.Snapshot.IssueComments[i].CreatedAt.Add(shift)
		source.Snapshot.IssueComments[i].UpdatedAt = source.Snapshot.IssueComments[i].UpdatedAt.Add(shift)
	}
	ref, err := f.captures.Save(t.Context(), f.a.ID, source)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := reviewprovider.ResolveHostedAllowance(reviewprovider.HostedAllowanceSnapshot{IssueComments: source.Snapshot.IssueComments, Complete: true, ObservedAt: source.Snapshot.ObservedAt}, source.Intent, source.PostingActor, source.ProviderActor, source.EarliestPosting)
	if err != nil || resolved.State != "available_observed" || time.Since(resolved.ResponseCreatedAt) < time.Minute || time.Since(source.Snapshot.ObservedAt) > time.Minute {
		t.Fatal("fixture did not distinguish response age from fetch age", resolved, err)
	}
	before := hostedBridgeState(t, f)
	raw := hostedBridgeBytes(t, f, ref)
	err = RetainHostedAllowance(t.Context(), f.captures, f.f.engine, f.a, f.anchor, 2, ref)
	assertCoreErrorCode(t, err, "validation_failed")
	if hostedBridgeState(t, f) != before || !bytes.Equal(raw, hostedBridgeBytes(t, f, ref)) || f.provider.posts != 1 || f.provider.reads != 1 {
		t.Fatal("old response created fresh allowance or changed source")
	}
	// The original independent source remains intact as well.
	if _, err = f.captures.Read(t.Context(), f.a.ID, got.Capture); err != nil {
		t.Fatal(err)
	}
	f.assertNotAdmitted(t)
}

func TestRetainHostedAllowanceCancellationAndMalformedArguments(t *testing.T) {
	f, got, _ := hostedBridgeObserved(t)
	before := hostedBridgeState(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := RetainHostedAllowance(ctx, f.captures, f.f.engine, f.a, f.anchor, 2, got.Capture); err == nil || hostedBridgeState(t, f) != before {
		t.Fatal("cancelled bridge retained successful preflight", err)
	}
	for _, err := range []error{
		RetainHostedAllowance(nil, f.captures, f.f.engine, f.a, f.anchor, 2, got.Capture),
		RetainHostedAllowance(t.Context(), nil, f.f.engine, f.a, f.anchor, 2, got.Capture),
		RetainHostedAllowance(t.Context(), f.captures, nil, f.a, f.anchor, 2, got.Capture),
	} {
		assertCoreErrorCode(t, err, "validation_failed")
	}
	if hostedBridgeState(t, f) != before || f.provider.posts != 1 || f.provider.reads != 1 {
		t.Fatal("invalid/cancelled arguments had effects")
	}
}

func TestRetainHostedAllowanceDifferentValidReferenceCannotRefreshOriginalLedger(t *testing.T) {
	f, got, source := hostedBridgeObserved(t)
	if err := RetainHostedAllowance(t.Context(), f.captures, f.f.engine, f.a, f.anchor, 2, got.Capture); err != nil {
		t.Fatal(err)
	}
	original := hostedBridgeBytes(t, f, got.Capture)
	// A later complete fetch of the same positive reply is a distinct immutable
	// observation. It cannot overwrite the already recorded allowance reference.
	source.Snapshot.ObservedAt = time.Now().UTC()
	source.FinishedAt = source.Snapshot.ObservedAt
	ref, err := f.captures.Save(t.Context(), f.a.ID, source)
	if err != nil {
		t.Fatal(err)
	}
	if ref == got.Capture {
		t.Fatal("fixture did not create distinct observation")
	}
	current := hostedBridgeBytes(t, f, ref)
	before := hostedBridgeState(t, f)
	err = RetainHostedAllowance(t.Context(), f.captures, f.f.engine, f.a, f.anchor, 2, ref)
	assertCoreErrorCode(t, err, "review_conflict")
	if hostedBridgeState(t, f) != before || !bytes.Equal(original, hostedBridgeBytes(t, f, got.Capture)) || !bytes.Equal(current, hostedBridgeBytes(t, f, ref)) || f.provider.posts != 1 || f.provider.reads != 1 {
		t.Fatal("different positive observation refreshed immutable preflight")
	}
	retained, err := f.f.engine.ReadHostedAllowance(t.Context(), f.f.project, f.a.ID)
	if err != nil || retained.CaptureID != got.Capture.ID || retained.CaptureSHA256 != got.Capture.SHA256 || retained.ObservedAt == source.Snapshot.ObservedAt.Format(time.RFC3339Nano) {
		t.Fatal("original allowance capture/timing replaced", retained, err)
	}
	f.assertNotAdmitted(t)
}
