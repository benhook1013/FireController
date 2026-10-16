package reviewruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func hostedRetainFixture(t *testing.T, wrongStoredIntent bool) (*cliEvidenceFixture, *HostedCaptureStore, reviewexec.Attempt, HostedCaptureRecord) {
	t.Helper()
	f := newCLIEvidenceFixture(t)
	raw, err := f.engine.Enqueue(t.Context(), f.scope(t, f.writer, false), "hosted-request", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "hosted", HeadSHA: f.headSHA})
	if err != nil {
		t.Fatal(err)
	}
	var receipt reviewexec.Receipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	attempt, err := f.engine.StartAttempt(t.Context(), f.project, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	triggerAt := time.Now().UTC().Add(-10 * time.Second).Truncate(time.Second)
	anchor := reviewexec.Anchor{RepositoryID: 901, Repository: "owner/repo", PullNumber: 2, CandidateSHA: f.headSHA, BaseSHA: f.baseSHA, ParentSHA: f.baseSHA, MergeBaseSHA: f.baseSHA, PatchSHA256: strings.Repeat("e", 64), BaseRef: "main", ObservedAt: triggerAt.Add(-time.Second).Format(time.RFC3339Nano)}
	intent := reviewprovider.HostedTriggerIntent{OperationID: attempt.OperationID, AttemptID: attempt.ID, HeadSHA: f.headSHA}
	body, err := reviewprovider.HostedTriggerBody(intent)
	if err != nil {
		t.Fatal(err)
	}
	storedIntent := body
	if wrongStoredIntent {
		storedIntent = "@coderabbitai review"
	}
	attempt, err = f.engine.Admit(t.Context(), f.project, attempt.ID, attempt.Revision, anchor, storedIntent)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = f.engine.Advance(t.Context(), f.project, attempt.ID, attempt.Revision, "active", "synthetic_posted", "100")
	if err != nil {
		t.Fatal(err)
	}
	captures, _ := newHostedTestStore(t)
	_, record := hostedCaptureFixture(t)
	record.Intent, record.EarliestPosting = intent, triggerAt.Add(-time.Second)
	// Polling happens after the review. Evidence must use provider trigger and
	// publication timing rather than this distinct observation window.
	record.StartedAt, record.FinishedAt = triggerAt.Add(4*time.Second), triggerAt.Add(6*time.Second)
	record.Snapshot.ObservedAt = triggerAt.Add(5 * time.Second)
	record.Snapshot.IssueComments[0].Body = body
	record.Snapshot.IssueComments[0].CreatedAt, record.Snapshot.IssueComments[0].UpdatedAt = triggerAt, triggerAt
	record.Snapshot.Reviews[0].HeadSHA = f.headSHA
	record.Snapshot.Reviews[0].SubmittedAt = triggerAt.Add(2 * time.Second)
	record.Snapshot.Reviews[0].Body = "**Actionable comments posted: 1**\n\n" + hostedRetainScope(f.baseSHA, f.headSHA) + "\n\n" + hostedRetainPublicationMarker()
	root := &record.Snapshot.ReviewComments[0]
	root.HeadSHA, root.OriginalHeadSHA = f.headSHA, f.headSHA
	root.CreatedAt, root.UpdatedAt = triggerAt.Add(time.Second), triggerAt.Add(time.Second)
	root.Body = hostedRetainInline(1, "Preserve exact evidence.")
	start, end := int64(6), int64(7)
	root.OriginalStartLine, root.OriginalLine = &start, &end
	root.Side, root.StartSide = "RIGHT", "RIGHT"
	return f, captures, attempt, record
}

func hostedRetainPublicationMarker() string {
	return "<!-- coderabbit-review-publication v1 publication=11111111-1111-1111-1111-111111111111 attempt=22222222-2222-2222-2222-222222222222 batch=1/1 -->"
}
func hostedRetainScope(base, head string) string {
	return "<details>\n<summary>ℹ️ Review info</summary>\nReviewing files that changed from the base of the PR and between " + base + " and " + head + ".\n</details>"
}
func hostedRetainInline(index int, title string) string {
	return "_⚠️ Potential issue_ | _🟠 Major_\n\n**" + title + "**\n\nLiteral <b>immutable detail</b> & source spacing.\n\n" + fmt.Sprintf("<!-- cr-comment:v1:%024x -->", index)
}
func hostedRetainClone(t *testing.T, r HostedCaptureRecord) HostedCaptureRecord {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var copy HostedCaptureRecord
	if err = json.Unmarshal(data, &copy); err != nil {
		t.Fatal(err)
	}
	return copy
}

func TestRetainHostedCompleteEvidenceIdentityTimingAndReplay(t *testing.T) {
	f, captures, attempt, record := hostedRetainFixture(t, false)
	before := cliAttemptState(t, f.store, attempt.ID)
	result, err := RetainHosted(t.Context(), captures, f.engine, attempt, record)
	if err != nil || !result.Complete || result.Reason != "capture_recorded" || result.Receipt.State != "captured" || !validHostedReference(result.Capture) {
		t.Fatalf("complete hosted source not retained: %+v %v", result, err)
	}
	loaded, err := captures.Read(t.Context(), attempt.ID, result.Capture)
	if err != nil || !reflect.DeepEqual(loaded, record) {
		t.Fatal("immutable private observation changed", err)
	}
	artifact := filepath.Join(captures.root, hostedCaptureFilename(attempt.ID, result.Capture))
	bytesBefore, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := f.engine.ReadReviewEvidence(t.Context(), f.scopeWithAccess(t, f.reader, false, "read"), result.Receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	capture := evidence.Capture
	if evidence.AttemptID != attempt.ID || evidence.OperationID != attempt.OperationID || evidence.Anchor != *attempt.Anchor || capture.CaptureID != result.Capture.ID || capture.CaptureSHA256 != result.Capture.SHA256 || capture.CaptureBytes != result.Capture.Bytes {
		t.Fatal("ledger lost exact operation/attempt/source/artifact binding")
	}
	if capture.StartedAt != record.Snapshot.IssueComments[0].CreatedAt.Format(time.RFC3339Nano) || capture.FinishedAt != record.Snapshot.Reviews[0].SubmittedAt.Format(time.RFC3339Nano) || capture.StartedAt == record.StartedAt.Format(time.RFC3339Nano) || capture.FinishedAt == record.FinishedAt.Format(time.RFC3339Nano) {
		t.Fatal("poll window replaced actual review timing")
	}
	if !capture.Review.Complete || capture.Review.Outcome != "completed" || capture.Review.ReportedFindings != 1 || capture.Review.ReviewedFiles == nil || len(capture.Review.ReviewedFiles) != 0 || len(capture.Review.Findings) != 1 || capture.Hosted == nil {
		t.Fatal("hosted inventory was confused with CLI reviewed files")
	}
	finding := capture.Review.Findings[0]
	if finding.Title != "Preserve exact evidence." || finding.Detail != record.Snapshot.ReviewComments[0].Body || finding.Severity != "high" || finding.Path != "source.go" || finding.StartLine != 6 || finding.EndLine != 7 {
		t.Fatalf("full finding fields changed: %+v", finding)
	}
	metadata := capture.Hosted
	if metadata.PublicationID != "11111111-1111-1111-1111-111111111111" || metadata.ProviderAttemptID != "22222222-2222-2222-2222-222222222222" || metadata.TriggerCommentID != 100 || !reflect.DeepEqual(metadata.ReviewIDs, []int64{200}) || metadata.ObservedBaseSHA != attempt.Anchor.ParentSHA || len(metadata.Sources) != 1 {
		t.Fatal("provider publication identity/comparison lost")
	}
	source := metadata.Sources[0]
	if source.SourceKey != "hosted-comment:300:"+fmt.Sprintf("%024x", 1) || source.Fingerprint != fmt.Sprintf("%024x", 1) || source.Kind != "inline" || source.ReviewID != 200 || source.CommentID != 300 || source.Side != "RIGHT" {
		t.Fatal("atomic source identity lost")
	}
	replay, err := RetainHosted(t.Context(), captures, f.engine, attempt, record)
	if err != nil || replay != result {
		t.Fatal("exact source replay changed receipt/ref", err)
	}
	bytesAfter, err := os.ReadFile(artifact)
	if err != nil || string(bytesBefore) != string(bytesAfter) || cliAttemptState(t, f.store, attempt.ID) != before {
		t.Fatal("retention rewrote raw observation or settled attempt", err)
	}
	if cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, attempt.ID) != 1 || cliCount(t, f.store, `SELECT count(*) FROM events WHERE resource_id=? AND event_type='review.captured'`, result.Receipt.ID) != 1 || cliCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.observed'`) != 0 {
		t.Fatal("retention duplicated evidence or published workspace")
	}
	encoded, _ := json.Marshal(result)
	for _, private := range []string{captures.root, record.Snapshot.ReviewComments[0].Body, "immutable detail"} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("compact retention result exposed source/path")
		}
	}
}

func TestRetainHostedPendingObservationsLeaveFinalEvidenceSlotFree(t *testing.T) {
	f, captures, attempt, complete := hostedRetainFixture(t, false)
	before := cliAttemptState(t, f.store, attempt.ID)
	changes := map[string]func(*HostedCaptureRecord){
		"markerless": func(r *HostedCaptureRecord) {
			r.Snapshot.Reviews[0].Body = strings.Replace(r.Snapshot.Reviews[0].Body, hostedRetainPublicationMarker(), "", 1)
		},
		"missing batches": func(r *HostedCaptureRecord) {
			r.Snapshot.Reviews[0].Body = strings.Replace(r.Snapshot.Reviews[0].Body, "batch=1/1", "batch=1/2", 1)
		},
		"ambiguous batches": func(r *HostedCaptureRecord) {
			copy := r.Snapshot.Reviews[0]
			copy.ID++
			r.Snapshot.Reviews = append(r.Snapshot.Reviews, copy)
		},
		"unsupported finding": func(r *HostedCaptureRecord) { r.Snapshot.ReviewComments[0].Body = "Unrecognized source finding prose." },
		"scope absent": func(r *HostedCaptureRecord) {
			r.Snapshot.Reviews[0].Body = "**Actionable comments posted: 1**\n" + hostedRetainPublicationMarker()
		},
		"scope unknown": func(r *HostedCaptureRecord) {
			r.Snapshot.Reviews[0].Body = strings.Replace(r.Snapshot.Reviews[0].Body, attempt.Anchor.ParentSHA, strings.Repeat("f", 40), 1)
		},
		"repository identity": func(r *HostedCaptureRecord) { r.RepositoryID++ },
		"repository name":     func(r *HostedCaptureRecord) { r.Repository = "other/repo" },
		"pull target":         func(r *HostedCaptureRecord) { r.PullNumber++ },
		"operation intent":    func(r *HostedCaptureRecord) { r.Intent.OperationID = "review_operation_" + strings.Repeat("f", 32) },
		"head intent":         func(r *HostedCaptureRecord) { r.Intent.HeadSHA = strings.Repeat("f", 40) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			record := hostedRetainClone(t, complete)
			change(&record)
			result, err := RetainHosted(t.Context(), captures, f.engine, attempt, record)
			if err != nil || result.Complete || result.Receipt.ID != "" || result.Reason == "" || !validHostedReference(result.Capture) {
				t.Fatalf("pending observation lost or acquired final evidence: %+v %v", result, err)
			}
			switch name {
			case "repository identity", "repository name", "pull target", "operation intent", "head intent":
				if result.Reason != "source_changed" {
					t.Fatal("wrong target/intent lost source_changed reason")
				}
			case "scope absent", "scope unknown":
				if result.Reason != "comparison_unproven" {
					t.Fatal("unproven scope acquired another reason")
				}
			}
			loaded, err := captures.Read(t.Context(), attempt.ID, result.Capture)
			if err != nil || !reflect.DeepEqual(loaded, record) {
				t.Fatal("held raw observation unavailable", err)
			}
			if cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, attempt.ID) != 0 || cliCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.captured'`) != 0 || cliAttemptState(t, f.store, attempt.ID) != before {
				t.Fatal("pending observation occupied final slot or mutated attempt")
			}
		})
	}
	// A later, supported complete observation for that same attempt can still
	// occupy its one immutable final-evidence slot after every earlier hold.
	result, err := RetainHosted(t.Context(), captures, f.engine, attempt, complete)
	if err != nil || !result.Complete || result.Receipt.ID == "" {
		t.Fatal("earlier pending observation blocked later final evidence", err)
	}
}

func TestRetainHostedAtomicFindingCountAndUnknownLines(t *testing.T) {
	for _, count := range []int{0, 2} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			f, captures, attempt, record := hostedRetainFixture(t, false)
			if count == 0 {
				record.Snapshot.Reviews[0].Body = strings.Replace(record.Snapshot.Reviews[0].Body, "posted: 1", "posted: 0", 1)
				record.Snapshot.ReviewComments = []reviewprovider.ReviewComment{}
			} else {
				record.Snapshot.ReviewComments[0].Body += "\n\n" + hostedRetainInline(2, "Keep another atomic finding.")
				root := &record.Snapshot.ReviewComments[0]
				root.Line, root.StartLine, root.OriginalLine, root.OriginalStartLine = nil, nil, nil, nil
			}
			result, err := RetainHosted(t.Context(), captures, f.engine, attempt, record)
			if err != nil || !result.Complete {
				t.Fatalf("valid zero/bundled inventory not retained: %+v %v", result, err)
			}
			evidence, err := f.engine.ReadReviewEvidence(t.Context(), f.scopeWithAccess(t, f.reader, false, "read"), result.Receipt.ID)
			if err != nil || len(evidence.Capture.Review.Findings) != count || len(evidence.Capture.Hosted.Sources) != count || evidence.Capture.Review.ReportedFindings != int64(count) || evidence.Capture.Hosted.Sources == nil {
				t.Fatal("root count substituted for atomic findings", err)
			}
			for i, finding := range evidence.Capture.Review.Findings {
				if finding.StartLine != 0 || finding.EndLine != 0 || evidence.Capture.Hosted.Sources[i].Fingerprint != fmt.Sprintf("%024x", i+1) {
					t.Fatal("unknown line or atomic identity invented")
				}
			}
		})
	}
}

func TestRetainHostedJournalRechecksStoredPostingFacts(t *testing.T) {
	for _, mismatch := range []string{"retained intent", "retained trigger", "forged caller anchor"} {
		t.Run(mismatch, func(t *testing.T) {
			f, captures, attempt, record := hostedRetainFixture(t, mismatch == "retained intent")
			if mismatch == "retained trigger" {
				// The capture names a different otherwise valid trigger. The
				// journal's immutable posted external ID must remain authoritative.
				record.Snapshot.IssueComments[0].ID = 101
			}
			if mismatch == "forged caller anchor" {
				copy := *attempt.Anchor
				copy.ParentSHA = strings.Repeat("f", 40)
				copy.MergeBaseSHA = copy.ParentSHA
				attempt.Anchor = &copy
				record.Snapshot.Reviews[0].Body = strings.ReplaceAll(record.Snapshot.Reviews[0].Body, f.baseSHA, copy.ParentSHA)
			}
			before := cliAttemptState(t, f.store, attempt.ID)
			result, err := RetainHosted(t.Context(), captures, f.engine, attempt, record)
			assertCoreErrorCode(t, err, "review_conflict")
			wantReason := "observation_unavailable"
			if mismatch == "retained trigger" {
				wantReason = "evidence_unavailable"
			}
			if result.Complete || result.Receipt.ID != "" || !validHostedReference(result.Capture) || result.Reason != wantReason {
				t.Fatal("journal rejection lost retained ref or claimed completion")
			}
			if _, err := captures.Read(t.Context(), attempt.ID, result.Capture); err != nil {
				t.Fatal("journal-rejected observation not inspectable", err)
			}
			if cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, attempt.ID) != 0 || cliAttemptState(t, f.store, attempt.ID) != before {
				t.Fatal("caller facts overrode retained authority")
			}
		})
	}
}

func TestRetainHostedInvalidArgumentsNeverPanicOrStoreUnderAnotherAttempt(t *testing.T) {
	f, captures, attempt, record := hostedRetainFixture(t, false)
	for _, invalid := range []string{"nil context", "nil store", "nil engine", "nil anchor", "bad operation", "bad project", "wrong record attempt", "incomplete collection"} {
		t.Run(invalid, func(t *testing.T) {
			ctx := context.Background()
			store := captures
			engine := f.engine
			usedAttempt := attempt
			usedRecord := hostedRetainClone(t, record)
			switch invalid {
			case "nil context":
				ctx = nil
			case "nil store":
				store = nil
			case "nil engine":
				engine = nil
			case "nil anchor":
				usedAttempt.Anchor = nil
			case "bad operation":
				usedAttempt.OperationID = "bad"
			case "bad project":
				usedAttempt.ProjectID = "../other"
			case "wrong record attempt":
				usedRecord.Intent.AttemptID = "review_attempt_" + strings.Repeat("f", 32)
			case "incomplete collection":
				usedRecord.Snapshot.Complete = false
			}
			result, err := RetainHosted(ctx, store, engine, usedAttempt, usedRecord)
			assertCoreErrorCode(t, err, "validation_failed")
			if result.Capture.ID != "" || result.Receipt.ID != "" || result.Complete {
				t.Fatal("invalid arguments acquired another attempt's artifact/evidence")
			}
		})
	}
	entries, err := os.ReadDir(captures.root)
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid calls stored capture artifacts", err)
	}
}

func TestRetainHostedObservationReferencesRecoverPendingAndComplete(t *testing.T) {
	f, captures, attempt, complete := hostedRetainFixture(t, false)
	before := cliAttemptState(t, f.store, attempt.ID)
	pending := hostedRetainClone(t, complete)
	pending.Snapshot.Reviews[0].Body = strings.Replace(pending.Snapshot.Reviews[0].Body, hostedRetainPublicationMarker(), "", 1)
	held, err := RetainHosted(t.Context(), captures, f.engine, attempt, pending)
	if err != nil || held.Complete || !validHostedReference(held.Capture) {
		t.Fatal("pending observation not retained", held, err)
	}
	refs, err := f.engine.ReadHostedObservations(t.Context(), f.project, attempt.ID)
	if err != nil || len(refs) != 1 || refs[0].Input.CaptureSHA256 != held.Capture.SHA256 || refs[0].Input.StartedAt != pending.StartedAt.Format(time.RFC3339Nano) || refs[0].Input.ObservedAt != pending.Snapshot.ObservedAt.Format(time.RFC3339Nano) || refs[0].Input.FinishedAt != pending.FinishedAt.Format(time.RFC3339Nano) {
		t.Fatal("pending reference missing measured observation facts", refs, err)
	}
	// Drop every process-local observation value. Recovery locates immutable raw
	// bytes using only the durable journal, without listing files or provider I/O.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = core.Open(t.Context(), f.storeDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.store.Close() })
	if err := reviewexec.EnsureEvidenceSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	f.engine, err = reviewexec.New(f.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	captures, err = NewHostedCaptureStore(captures.root)
	if err != nil {
		t.Fatal(err)
	}
	refs, err = f.engine.ReadHostedObservations(t.Context(), f.project, attempt.ID)
	if err != nil || len(refs) != 1 {
		t.Fatal("restart lost pending source reference", err)
	}
	reference := CaptureReference{ID: refs[0].Input.CaptureID, SHA256: refs[0].Input.CaptureSHA256, Bytes: refs[0].Input.CaptureBytes}
	recovered, err := captures.Read(t.Context(), attempt.ID, reference)
	if err != nil || !reflect.DeepEqual(recovered, pending) {
		t.Fatal("journal could not locate exact pending source", err)
	}
	heldReplay, err := RetainHosted(t.Context(), captures, f.engine, attempt, recovered)
	if err != nil || heldReplay != held {
		t.Fatal("restart pending replay changed capture", err)
	}
	final, err := RetainHosted(t.Context(), captures, f.engine, attempt, complete)
	if err != nil || !final.Complete {
		t.Fatal("later complete source blocked by pending reference", final, err)
	}
	refs, err = f.engine.ReadHostedObservations(t.Context(), f.project, attempt.ID)
	if err != nil || len(refs) != 2 || refs[0].Input.CaptureSHA256 != held.Capture.SHA256 || refs[1].Input.CaptureSHA256 != final.Capture.SHA256 || refs[1].Position != 2 {
		t.Fatal("pending and complete references not retained independently", err)
	}
	finalReplay, err := RetainHosted(t.Context(), captures, f.engine, attempt, complete)
	if err != nil || finalReplay != final || cliCount(t, f.store, `SELECT count(*) FROM review_hosted_observations WHERE attempt_id=?`, attempt.ID) != 2 || cliCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.capture_observed'`) != 2 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, attempt.ID) != 1 || cliAttemptState(t, f.store, attempt.ID) != before {
		t.Fatal("reference replay duplicated effects/settled attempt", err)
	}
	// Both source references and the unique final row survive a second restart.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = core.Open(t.Context(), f.storeDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.store.Close() })
	f.engine, err = reviewexec.New(f.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	refs, err = f.engine.ReadHostedObservations(t.Context(), f.project, attempt.ID)
	if err != nil || len(refs) != 2 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, attempt.ID) != 1 {
		t.Fatal("restart lost observation/final evidence distinction", err)
	}
}
