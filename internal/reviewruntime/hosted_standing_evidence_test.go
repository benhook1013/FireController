package reviewruntime

import (
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

const runtimeStandingRun = "abcdef12-2222-4333-8444-555555555555"

// Synthetic protocol fields only; the mutable summary has its real comment
// identity and never supplies a fabricated review batch.
func runtimeStandingBody(base, head string) string {
	block := func(label, body string) string {
		return "<details>\n<summary><strong>" + label + "</strong></summary>\n<dl>\n<dd>\n" + body + "\n</dd>\n</dl>\n</details>"
	}
	configuration := block("⚙️ Run configuration", "- **Configuration used**: Repository: synthetic/project/.coderabbit.yaml\n- **Review profile**: ASSERTIVE\n- **Plan**: Advanced\n- **Run ID**: `"+runtimeStandingRun+"`")
	commits := block("📥 Commits", "Reviewing files that changed from the base of the PR and between "+base+" and "+head+".")
	ignored := block("⛔ Files ignored due to path filters: 0", "")
	selected := block("📒 Files selected for processing: 1", "* `pkg/source.go`")
	return "<!-- This is an auto-generated comment: summarize by coderabbit.ai -->\n<!-- recent_review_start -->\nNo actionable comments were generated in the recent review. 🎉\n" + block("ℹ️ Recent review info", configuration+"\n"+commits+"\n"+ignored+"\n"+selected) + "\n<!-- recent_review_end -->\n<details>\n<summary><strong>Merge Risk:</strong> <strong>🔵 Low</strong> · up to <code>" + head[:5] + "</code></summary>\n<dl>\n<dd>\n<!-- final_review_risk_start -->\n<!-- final_review_risk_coverage:{\"sourceCommitId\":\"" + head + "\",\"coveredCommitId\":\"" + head + "\",\"kind\":\"reviewed\"} -->\n<!-- final_review_risk_end -->\n</dd>\n</dl>\n</details>"
}

// Create the confirmed query before admission through the owning Engine APIs.
// Existing runtime fixtures provide real SQLite, capability and private storage.
func runtimeStandingFixture(t *testing.T, baselineEdit func(*HostedCaptureRecord), inputEdit func(*reviewexec.HostedObservationInput)) (*runtimeAllowanceFixture, HostedCaptureRecord) {
	t.Helper()
	f := newRuntimeAllowanceFixture(t)
	query, created, err := f.f.engine.BeginHostedAllowanceQuery(t.Context(), f.f.project, f.a.ID, f.a.Revision, f.anchor, f.provider.poster, f.provider.bot)
	if err != nil || !created {
		t.Fatal("begin query", err)
	}
	at := time.Now().UTC()
	comment := reviewprovider.IssueComment{ID: 80, Body: query.Intent, Author: query.PostingActor, CreatedAt: at, UpdatedAt: at}
	query, err = f.f.engine.ConfirmHostedAllowanceQuery(t.Context(), f.f.project, f.a.ID, query.Revision, comment)
	if err != nil {
		t.Fatal(err)
	}
	earliest, err := time.Parse(time.RFC3339Nano, query.EarliestPosting)
	if err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC().Add(time.Nanosecond)
	baseline := HostedCaptureRecord{RepositoryID: f.anchor.RepositoryID, Repository: f.anchor.Repository, PullNumber: 2, Intent: reviewprovider.HostedTriggerIntent{OperationID: f.a.OperationID, AttemptID: f.a.ID, HeadSHA: f.f.headSHA}, PostingActor: query.PostingActor, ProviderActor: query.ProviderActor, EarliestPosting: earliest.Truncate(time.Second), StartedAt: at, FinishedAt: observed,
		Snapshot: reviewprovider.HostedPublicationSnapshot{Complete: true, ObservedAt: observed, Reviews: []reviewprovider.HostedReview{}, ReviewComments: []reviewprovider.ReviewComment{}, IssueComments: []reviewprovider.IssueComment{comment, {ID: 81, Body: runtimeAllowanceAvailable, Author: query.ProviderActor, CreatedAt: at.Add(time.Nanosecond), UpdatedAt: at.Add(time.Nanosecond)}}}}
	if baselineEdit != nil {
		baselineEdit(&baseline)
	}
	ref, saveErr := f.captures.Save(t.Context(), f.a.ID, baseline)
	if !baseline.Snapshot.Complete {
		if saveErr == nil {
			t.Fatal("incomplete baseline was retained")
		}
	} else {
		if saveErr != nil {
			t.Fatal("save baseline", saveErr)
		}
		in := reviewexec.HostedObservationInput{Kind: "allowance_query", CaptureID: ref.ID, CaptureSHA256: ref.SHA256, CaptureBytes: ref.Bytes, StartedAt: baseline.StartedAt.Format(time.RFC3339Nano), FinishedAt: baseline.FinishedAt.Format(time.RFC3339Nano), ObservedAt: baseline.Snapshot.ObservedAt.Format(time.RFC3339Nano), ExpectedAnchor: query.Anchor, Intent: query.Intent}
		if inputEdit != nil {
			inputEdit(&in)
		}
		if _, err = f.f.engine.RecordHostedObservation(t.Context(), f.f.project, f.a.ID, in); err != nil {
			t.Fatal("journal baseline", err)
		}
	}
	intent := reviewprovider.HostedTriggerIntent{OperationID: f.a.OperationID, AttemptID: f.a.ID, HeadSHA: f.f.headSHA}
	body, err := reviewprovider.HostedTriggerBody(intent)
	if err != nil {
		t.Fatal(err)
	}
	f.a, err = f.f.engine.Admit(t.Context(), f.f.project, f.a.ID, f.a.Revision, f.anchor, body)
	if err != nil {
		t.Fatal(err)
	}
	admittedAt, err := f.f.engine.ReadHostedPostingBoundary(t.Context(), f.f.project, f.a.ID)
	if err != nil {
		t.Fatal(err)
	}
	triggerAt := admittedAt.Truncate(time.Second)
	f.a, err = f.f.engine.Advance(t.Context(), f.f.project, f.a.ID, f.a.Revision, "active", "synthetic_posted", "100")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := reviewprovider.HostedPublicationSnapshot{Complete: true, ObservedAt: triggerAt.Add(3 * time.Second), Reviews: []reviewprovider.HostedReview{}, ReviewComments: []reviewprovider.ReviewComment{}, IssueComments: []reviewprovider.IssueComment{
		{ID: 100, Body: body, Author: query.PostingActor, CreatedAt: triggerAt, UpdatedAt: triggerAt},
		{ID: 501, Body: runtimeStandingBody(f.f.baseSHA, f.f.headSHA), Author: query.ProviderActor, CreatedAt: triggerAt.Add(-time.Hour), UpdatedAt: triggerAt.Add(time.Second)},
		{ID: 502, Body: "<!-- This is an auto-generated reply by CodeRabbit -->\n<!-- CodeRabbit review command invocation: v2:" + strings.Repeat("c", 64) + " -->\n<details>\n<summary>✅ Action performed</summary>\nFull review finished.\n</details>", Author: query.ProviderActor, CreatedAt: triggerAt.Add(2 * time.Second), UpdatedAt: triggerAt.Add(2 * time.Second)},
	}}
	record := HostedCaptureRecord{RepositoryID: f.anchor.RepositoryID, Repository: f.anchor.Repository, PullNumber: 2, Intent: intent, PostingActor: query.PostingActor, ProviderActor: query.ProviderActor, EarliestPosting: triggerAt, StartedAt: triggerAt, FinishedAt: snapshot.ObservedAt, Snapshot: snapshot}
	inventory, err := reviewprovider.ExtractHostedFindings(snapshot, intent, record.PostingActor, record.ProviderActor, record.EarliestPosting)
	if err != nil || !inventory.Complete || inventory.Publication.StandingZero == nil {
		t.Fatalf("synthetic standing protocol invalid: %+v %v", inventory, err)
	}
	return f, record
}

func TestRetainHostedStandingZeroBaselineIdentityAndFinalization(t *testing.T) {
	f, record := runtimeStandingFixture(t, nil, nil)
	before := cliAttemptState(t, f.f.store, f.a.ID)
	result, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || !result.Complete || result.Reason != "capture_recorded" {
		t.Fatalf("standing not recorded: %+v %v", result, err)
	}
	evidence, err := f.f.engine.ReadReviewEvidence(t.Context(), f.f.scopeWithAccess(t, f.f.reader, false, "read"), result.Receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	h := evidence.Capture.Hosted
	want := &reviewexec.HostedStandingSummaryMetadata{CommentID: 501, ProviderRunID: runtimeStandingRun, UpdatedAt: record.Snapshot.IssueComments[1].UpdatedAt.Format(time.RFC3339Nano)}
	if h == nil || !reflect.DeepEqual(h.StandingSummary, want) || h.PublicationID != "" || h.ProviderAttemptID != "" || h.TriggerCommentID != 100 || h.ReviewIDs == nil || len(h.ReviewIDs) != 0 || h.Sources == nil || len(h.Sources) != 0 || evidence.Capture.Review.Findings == nil || len(evidence.Capture.Review.Findings) != 0 || evidence.Capture.StartedAt != record.Snapshot.IssueComments[0].CreatedAt.Format(time.RFC3339Nano) || evidence.Capture.FinishedAt != record.Snapshot.IssueComments[2].UpdatedAt.Format(time.RFC3339Nano) {
		t.Fatal("invented batch identity or lost actual summary/timing", h)
	}
	if cliAttemptState(t, f.f.store, f.a.ID) != before {
		t.Fatal("retainer settled attempt")
	}
	zero := int64(0)
	if _, err = f.f.engine.FinalizeReviewEvidence(t.Context(), f.f.scope(t, f.f.writer, false), "premature-standing-finalize", result.Receipt.ID, reviewexec.FinalizeReviewRequest{ExpectedRevision: &zero}); err == nil {
		t.Fatal("active attempt finalized")
	}
	f.a, err = f.f.engine.Advance(t.Context(), f.f.project, f.a.ID, f.a.Revision, "completed", "synthetic_complete", f.a.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.f.engine.FinalizeReviewEvidence(t.Context(), f.f.scope(t, f.f.writer, false), "standing-finalize", result.Receipt.ID, reviewexec.FinalizeReviewRequest{ExpectedRevision: &zero}); err != nil {
		t.Fatal(err)
	}
	replay, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || replay != result {
		t.Fatal("settled exact replay changed immutable source", replay, err)
	}
	journal, err := f.f.engine.ReadHostedObservations(t.Context(), f.f.project, f.a.ID)
	if err != nil || len(journal) != 2 || journal[0].Input.Kind != "allowance_query" {
		t.Fatal("query baseline/final source not durably auditable", journal, err)
	}
}

func TestRetainHostedStandingZeroRefusesUnprovenBaseline(t *testing.T) {
	for _, mode := range []string{"missing", "wrong_boundary", "query_only", "missing_issue_collection", "wrong_repository", "wrong_pull", "wrong_intent", "posting_actor", "provider_actor", "incomplete", "query_missing", "query_edited", "journal_time"} {
		t.Run(mode, func(t *testing.T) {
			edit := func(r *HostedCaptureRecord) {
				switch mode {
				case "query_only":
					r.Snapshot.Reviews, r.Snapshot.ReviewComments = nil, nil
				case "missing_issue_collection":
					r.Snapshot.IssueComments = nil
				case "wrong_repository":
					r.Repository = "other/repo"
				case "wrong_pull":
					r.PullNumber = 3
				case "wrong_intent":
					r.Intent.HeadSHA = strings.Repeat("f", 40)
				case "posting_actor":
					r.PostingActor.ID = 999
				case "provider_actor":
					r.ProviderActor.ID = 999
				case "incomplete":
					r.Snapshot.Complete = false
				case "query_missing":
					r.Snapshot.IssueComments = r.Snapshot.IssueComments[1:]
				case "query_edited":
					r.Snapshot.IssueComments[0].Body += " edited"
				}
			}
			inputEdit := func(in *reviewexec.HostedObservationInput) {
				if mode == "journal_time" {
					v, _ := time.Parse(time.RFC3339Nano, in.StartedAt)
					in.StartedAt = v.Add(-time.Nanosecond).Format(time.RFC3339Nano)
				}
			}
			f, record := runtimeStandingFixture(t, edit, inputEdit)
			if mode == "missing" {
				if _, err := f.f.store.DB().Exec(`DELETE FROM review_hosted_observations WHERE attempt_id=?`, f.a.ID); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "wrong_boundary" {
				record.EarliestPosting = record.EarliestPosting.Add(-time.Second)
			}
			before := cliAttemptState(t, f.f.store, f.a.ID)
			result, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
			if err != nil || result.Complete || result.Reason != "standing_baseline_unproven" || !validHostedReference(result.Capture) {
				t.Fatalf("unproven baseline gained credit: %+v %v", result, err)
			}
			loaded, err := f.captures.Read(t.Context(), f.a.ID, result.Capture)
			if err != nil || !reflect.DeepEqual(loaded, record) || cliAttemptState(t, f.f.store, f.a.ID) != before || cliCount(t, f.f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, f.a.ID) != 0 {
				t.Fatal("held source missing or state/evidence changed", err)
			}
		})
	}
}

func TestRetainHostedStandingZeroRunAbsentFromEveryProviderSource(t *testing.T) {
	for _, kind := range []string{"issue", "review", "inline", "foreign"} {
		t.Run(kind, func(t *testing.T) {
			f, record := runtimeStandingFixture(t, func(r *HostedCaptureRecord) {
				body := "Earlier provider Run ID " + strings.ToUpper(runtimeStandingRun)
				at := r.StartedAt
				switch kind {
				case "issue":
					r.Snapshot.IssueComments[1].Body = body
				case "review":
					r.Snapshot.Reviews = []reviewprovider.HostedReview{{ID: 601, Body: body, State: "commented", HeadSHA: r.Intent.HeadSHA, Author: r.ProviderActor, SubmittedAt: at}}
				case "inline":
					r.Snapshot.ReviewComments = []reviewprovider.ReviewComment{{ID: 602, ReviewID: 601, Body: body, Author: r.ProviderActor, Path: "source.go", HeadSHA: r.Intent.HeadSHA, OriginalHeadSHA: r.Intent.HeadSHA, CreatedAt: at, UpdatedAt: at}}
				case "foreign":
					r.Snapshot.IssueComments = append(r.Snapshot.IssueComments, reviewprovider.IssueComment{ID: 603, Body: body, Author: reviewprovider.Author{ID: 999, Kind: "Bot", Login: "unrelated"}, CreatedAt: at, UpdatedAt: at})
				}
			}, nil)
			result, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
			if kind == "foreign" {
				if err != nil || !result.Complete {
					t.Fatal("foreign prose supplied provider run identity", result, err)
				}
			} else if err != nil || result.Complete || result.Reason != "standing_run_not_new" || cliCount(t, f.f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, f.a.ID) != 0 {
				t.Fatal("old run identity gained fresh credit", result, err)
			}
		})
	}
}

// Keep the final fixture proof independent of JSON serializer mutation.
func TestRetainHostedStandingZeroMissingQueryRetainsRaw(t *testing.T) {
	f, captures, attempt, record := hostedRetainFixture(t, false)
	record.Snapshot.Reviews = []reviewprovider.HostedReview{}
	record.Snapshot.ReviewComments = []reviewprovider.ReviewComment{}
	trigger := record.Snapshot.IssueComments[0].CreatedAt
	record.Snapshot.IssueComments = append(record.Snapshot.IssueComments,
		reviewprovider.IssueComment{ID: 501, Body: runtimeStandingBody(f.baseSHA, f.headSHA), Author: record.ProviderActor, CreatedAt: trigger.Add(-time.Hour), UpdatedAt: trigger.Add(time.Second)},
		reviewprovider.IssueComment{ID: 502, Body: "<!-- This is an auto-generated reply by CodeRabbit -->\n<!-- CodeRabbit review command invocation: v2:" + strings.Repeat("c", 64) + " -->\n<details>\n<summary>✅ Action performed</summary>\nFull review finished.\n</details>", Author: record.ProviderActor, CreatedAt: trigger.Add(2 * time.Second), UpdatedAt: trigger.Add(2 * time.Second)})
	result, err := RetainHosted(t.Context(), captures, f.engine, attempt, record)
	if err != nil || result.Complete || result.Reason != "standing_baseline_unproven" {
		t.Fatal(result, err)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), runtimeStandingRun) || strings.Contains(string(encoded), captures.root) {
		t.Fatal("held diagnostic leaked private source")
	}
}

func TestRetainHostedStandingZeroLatestPrePostJournalOwnsBaseline(t *testing.T) {
	for _, mode := range []string{"new_run_in_latest", "invalid_latest", "latest_clears_old_run", "tampered_latest"} {
		t.Run(mode, func(t *testing.T) {
			f, record := runtimeStandingFixture(t, func(r *HostedCaptureRecord) {
				if mode == "latest_clears_old_run" {
					r.Snapshot.IssueComments[1].Body += " old " + runtimeStandingRun
				}
			}, nil)
			journal, err := f.f.engine.ReadHostedObservations(t.Context(), f.f.project, f.a.ID)
			if err != nil || len(journal) != 1 {
				t.Fatal(journal, err)
			}
			in := journal[0].Input
			latest, err := f.captures.Read(t.Context(), f.a.ID, CaptureReference{ID: in.CaptureID, SHA256: in.CaptureSHA256, Bytes: in.CaptureBytes})
			if err != nil {
				t.Fatal(err)
			}
			// Later journal position is authoritative even if its fetch timestamps are
			// earlier. A timestamp/id sort must not resurrect a stale mutable body.
			latest.Snapshot.ObservedAt = latest.Snapshot.IssueComments[1].UpdatedAt
			latest.FinishedAt = latest.Snapshot.ObservedAt
			latest.Snapshot.IssueComments[1].Body = runtimeAllowanceAvailable
			if mode == "new_run_in_latest" {
				latest.Snapshot.IssueComments[1].Body += " prior " + strings.ToUpper(runtimeStandingRun)
			}
			if mode == "invalid_latest" {
				latest.Repository = "unrelated/repo"
			}
			ref, err := f.captures.Save(t.Context(), f.a.ID, latest)
			if err != nil {
				t.Fatal(err)
			}
			in.CaptureID, in.CaptureSHA256, in.CaptureBytes = ref.ID, ref.SHA256, ref.Bytes
			in.FinishedAt, in.ObservedAt = latest.FinishedAt.Format(time.RFC3339Nano), latest.Snapshot.ObservedAt.Format(time.RFC3339Nano)
			if _, err = f.f.engine.RecordHostedObservation(t.Context(), f.f.project, f.a.ID, in); err != nil {
				t.Fatal(err)
			}
			if mode == "tampered_latest" {
				path := filepath.Join(f.captures.root, hostedCaptureFilename(f.a.ID, ref))
				if err = os.WriteFile(path, []byte("tampered immutable capture"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
			if mode == "latest_clears_old_run" {
				if err != nil || !result.Complete {
					t.Fatal("older baseline overrode latest source", result, err)
				}
				return
			}
			expected := "standing_baseline_unproven"
			if mode == "new_run_in_latest" {
				expected = "standing_run_not_new"
			}
			if result.Complete || result.Reason != expected || (err != nil) != (mode == "tampered_latest") || !validHostedReference(result.Capture) || cliCount(t, f.f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, f.a.ID) != 0 {
				t.Fatal("latest invalid/repeated/tampered source fell back", result, err)
			}
			if _, readErr := f.captures.Read(t.Context(), f.a.ID, result.Capture); readErr != nil {
				t.Fatal("held final raw capture missing", readErr)
			}
		})
	}
}

func TestRetainHostedStandingZeroExactLocalAdmissionBoundary(t *testing.T) {
	for _, mode := range []string{"before", "equal", "after", "corrupt", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			f, record := runtimeStandingFixture(t, nil, nil)
			journal, err := f.f.engine.ReadHostedObservations(t.Context(), f.f.project, f.a.ID)
			if err != nil || len(journal) != 1 {
				t.Fatal(journal, err)
			}
			finished, err := time.Parse(time.RFC3339Nano, journal[0].Input.FinishedAt)
			if err != nil {
				t.Fatal(err)
			}
			// Fixed one-nanosecond separation demonstrates local ordering in the same
			// second. No sleeps, provider timestamp guesses, or future-second padding.
			admitted := finished.Add(time.Nanosecond)
			if mode == "equal" {
				admitted = finished
			}
			if mode == "after" {
				admitted = finished.Add(-time.Nanosecond)
			}
			if _, err = f.f.store.DB().Exec(`UPDATE review_execution_transitions SET created_at=? WHERE attempt_id=? AND revision=2`, admitted.Format(time.RFC3339Nano), f.a.ID); err != nil {
				t.Fatal(err)
			}
			floor := admitted.Truncate(time.Second)
			delta := floor.Sub(record.EarliestPosting)
			record.EarliestPosting = floor
			record.StartedAt = record.StartedAt.Add(delta)
			record.FinishedAt = record.FinishedAt.Add(delta)
			record.Snapshot.ObservedAt = record.Snapshot.ObservedAt.Add(delta)
			for i := range record.Snapshot.IssueComments {
				record.Snapshot.IssueComments[i].CreatedAt = record.Snapshot.IssueComments[i].CreatedAt.Add(delta)
				record.Snapshot.IssueComments[i].UpdatedAt = record.Snapshot.IssueComments[i].UpdatedAt.Add(delta)
			}
			if mode == "corrupt" {
				if _, err = f.f.store.DB().Exec(`UPDATE review_execution_transitions SET created_at='bad' WHERE attempt_id=? AND revision=2`, f.a.ID); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "duplicate" {
				if _, err = f.f.store.DB().Exec(`INSERT INTO review_execution_transitions(attempt_id,revision,state,reason,external_id,created_at) VALUES(?,99,'posting','admitted','',?)`, f.a.ID, admitted.Format(time.RFC3339Nano)); err != nil {
					t.Fatal(err)
				}
			}
			result, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
			if mode == "before" || mode == "equal" {
				if err != nil || !result.Complete {
					t.Fatal("pre-admission same-second capture rejected", result, err)
				}
				return
			}
			if result.Complete || result.Reason != "standing_baseline_unproven" || (err != nil) != (mode == "corrupt" || mode == "duplicate") || cliCount(t, f.f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, f.a.ID) != 0 {
				t.Fatal("post-admission/corrupt boundary granted credit", result, err)
			}
			if _, readErr := f.captures.Read(t.Context(), f.a.ID, result.Capture); readErr != nil {
				t.Fatal("raw observation lost", readErr)
			}
		})
	}
}
