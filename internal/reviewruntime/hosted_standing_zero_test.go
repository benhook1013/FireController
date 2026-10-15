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

// Independently authored controlled provider shape; this is not a live review.
func runtimeStandingZeroBody(base, head, run string) string {
	return "<!-- recent_review_start -->\nNo actionable comments were generated in the recent review. 🎉\n<details>\n<summary><strong>ℹ️ Recent review info</strong></summary>\n<dl>\n<dd>\n<details>\n<summary><strong>⚙️ Run configuration</strong></summary>\n- **Run ID**: `" + run + "`\n</details>\n<details>\n<summary><strong>📥 Commits</strong></summary>\nReviewing files that changed from the base of the PR and between " + base + " and " + head + ".\n</details>\n</dd>\n</dl>\n</details>\n<!-- recent_review_end -->"
}

func runtimeStandingZeroFixture(t *testing.T) (*cliEvidenceFixture, *HostedCaptureStore, reviewexec.Attempt, HostedCaptureRecord, HostedCaptureRecord) {
	t.Helper()
	f, captures, attempt, record := hostedRetainFixture(t, false)
	trigger := record.Snapshot.IssueComments[0]
	at := trigger.CreatedAt
	record.Snapshot.Reviews = []reviewprovider.HostedReview{}
	record.Snapshot.ReviewComments = []reviewprovider.ReviewComment{}
	standing := reviewprovider.IssueComment{ID: 101, Author: record.ProviderActor, CreatedAt: at.Add(-time.Minute), UpdatedAt: at.Add(-time.Second), Body: runtimeStandingZeroBody(f.baseSHA, f.headSHA, "11111111-1111-1111-1111-111111111111")}
	record.Snapshot.IssueComments = []reviewprovider.IssueComment{trigger, standing}
	record.StartedAt, record.Snapshot.ObservedAt, record.FinishedAt = at.Add(time.Second), at.Add(2*time.Second), at.Add(3*time.Second)
	final := hostedRetainClone(t, record)
	final.Snapshot.IssueComments[1].UpdatedAt = at.Add(4 * time.Second)
	final.Snapshot.IssueComments[1].Body = runtimeStandingZeroBody(f.baseSHA, f.headSHA, "22222222-2222-2222-2222-222222222222")
	final.Snapshot.IssueComments = append(final.Snapshot.IssueComments, reviewprovider.IssueComment{ID: 102, Author: record.ProviderActor, Body: "Full review finished.", CreatedAt: at.Add(5 * time.Second), UpdatedAt: at.Add(5 * time.Second)})
	final.StartedAt, final.Snapshot.ObservedAt, final.FinishedAt = at.Add(6*time.Second), at.Add(7*time.Second), at.Add(8*time.Second)
	return f, captures, attempt, record, final
}

func TestRetainHostedStandingZeroPendingBaselineFinalAndRestartReplay(t *testing.T) {
	f, captures, attempt, pending, final := runtimeStandingZeroFixture(t)
	initial := cliAttemptState(t, f.store, attempt.ID)
	baseline, err := RetainHosted(t.Context(), captures, f.engine, attempt, pending)
	if err != nil || baseline.Complete || baseline.Receipt != (reviewexec.Receipt{}) {
		t.Fatal("pending source became final evidence", baseline, err)
	}
	result, err := RetainHosted(t.Context(), captures, f.engine, attempt, final)
	if err != nil || !result.Complete || result.Reason != "capture_recorded" {
		t.Fatal("controlled zero source not recorded", result, err)
	}
	evidence, err := f.engine.ReadReviewEvidence(t.Context(), f.scopeWithAccess(t, f.reader, false, "read"), result.Receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	capture := evidence.Capture
	if capture.Hosted.StandingZero == nil || capture.Hosted.StandingZero.CommentID != 101 || capture.Hosted.StandingZero.CompletionCommentID != 102 || capture.Hosted.StandingZero.RunID != "22222222-2222-2222-2222-222222222222" || capture.Hosted.StandingZero.BaselineCaptureID != baseline.Capture.ID || capture.Hosted.StandingZero.BaselineCaptureSHA256 != baseline.Capture.SHA256 || capture.Hosted.PublicationID != "" || capture.Hosted.ProviderAttemptID != "" || len(capture.Hosted.ReviewIDs) != 0 || capture.Hosted.Sources == nil || len(capture.Hosted.Sources) != 0 || capture.Review.Findings == nil || len(capture.Review.Findings) != 0 || capture.Review.ReportedFindings != 0 || capture.Review.ReviewedFiles == nil || len(capture.Review.ReviewedFiles) != 0 {
		t.Fatal("lost real identities or fabricated batch/finding inventory", capture)
	}
	if capture.StartedAt != final.Snapshot.IssueComments[0].CreatedAt.Format(time.RFC3339Nano) || capture.FinishedAt != final.Snapshot.IssueComments[2].UpdatedAt.Format(time.RFC3339Nano) {
		t.Fatal("local polling window replaced provider review timing")
	}
	for _, pair := range []struct {
		ref    CaptureReference
		record HostedCaptureRecord
	}{{baseline.Capture, pending}, {result.Capture, final}} {
		loaded, err := captures.Read(t.Context(), attempt.ID, pair.ref)
		if err != nil || !reflect.DeepEqual(loaded, pair.record) {
			t.Fatal("raw source changed", err)
		}
	}
	observations, err := f.engine.ReadHostedObservations(t.Context(), f.project, attempt.ID)
	if err != nil || len(observations) != 2 {
		t.Fatal(observations, err)
	}
	before := cliCount(t, f.store, `SELECT count(*) FROM events`)
	reopened, err := NewHostedCaptureStore(captures.root)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "runtime-capability", Provider: "coderabbit", Account: "runtime-account", Channels: []string{"cli", "hosted"}}})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := RetainHosted(t.Context(), reopened, engine, attempt, final)
	if err != nil || replay != result || cliAttemptState(t, f.store, attempt.ID) != initial || cliCount(t, f.store, `SELECT count(*) FROM events`) != before || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, attempt.ID) != 1 {
		t.Fatal("restart replay changed journal/reservation/evidence", replay, err)
	}
	observations, err = engine.ReadHostedObservations(t.Context(), f.project, attempt.ID)
	if err != nil || len(observations) != 2 {
		t.Fatal("replay added observation", err)
	}
	entries, err := os.ReadDir(captures.root)
	if err != nil || len(entries) != 2 {
		t.Fatal("replay added artifact", err)
	}
	compact, _ := json.Marshal(result)
	if strings.Contains(string(compact), "Run configuration") || strings.Contains(string(compact), captures.root) {
		t.Fatal("raw provider text/path leaked")
	}
}

func TestRetainHostedStandingZeroCausalityHoldsKeepFinalSlotFree(t *testing.T) {
	for _, mode := range []string{"no baseline", "same run", "baseline already finished", "post-finish edit", "earliest cannot be replaced", "corrupt baseline"} {
		t.Run(mode, func(t *testing.T) {
			f, captures, attempt, pending, final := runtimeStandingZeroFixture(t)
			if mode == "same run" || mode == "earliest cannot be replaced" {
				pending.Snapshot.IssueComments[1].Body = final.Snapshot.IssueComments[1].Body
			}
			if mode == "baseline already finished" {
				pending.Snapshot.IssueComments = append(pending.Snapshot.IssueComments, final.Snapshot.IssueComments[2])
				pending.StartedAt = final.StartedAt
				pending.Snapshot.ObservedAt = final.Snapshot.ObservedAt
				pending.FinishedAt = final.FinishedAt
			}
			if mode == "post-finish edit" {
				final.Snapshot.IssueComments[1].UpdatedAt = final.Snapshot.IssueComments[2].UpdatedAt.Add(time.Second)
			}
			var baseline CapturedHosted
			if mode != "no baseline" {
				var err error
				baseline, err = RetainHosted(t.Context(), captures, f.engine, attempt, pending)
				if err != nil || baseline.Complete {
					t.Fatal(baseline, err)
				}
			}
			if mode == "earliest cannot be replaced" {
				better := hostedRetainClone(t, pending)
				better.Snapshot.IssueComments[1].Body = runtimeStandingZeroBody(f.baseSHA, f.headSHA, "33333333-3333-3333-3333-333333333333")
				better.Snapshot.ObservedAt = better.Snapshot.ObservedAt.Add(100 * time.Millisecond)
				if r, err := RetainHosted(t.Context(), captures, f.engine, attempt, better); err != nil || r.Complete {
					t.Fatal(r, err)
				}
			}
			if mode == "corrupt baseline" {
				if err := os.WriteFile(filepath.Join(captures.root, hostedCaptureFilename(attempt.ID, baseline.Capture)), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := cliAttemptState(t, f.store, attempt.ID)
			result, err := RetainHosted(t.Context(), captures, f.engine, attempt, final)
			if mode == "corrupt baseline" {
				if err == nil {
					t.Fatal("corrupt baseline hidden")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if result.Complete || result.Receipt != (reviewexec.Receipt{}) || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, attempt.ID) != 0 || cliAttemptState(t, f.store, attempt.ID) != before {
				t.Fatal("unproven causality settled/granted evidence", result)
			}
			if _, err := captures.Read(t.Context(), attempt.ID, result.Capture); err != nil {
				t.Fatal("held current raw source unavailable", err)
			}
			if mode != "corrupt baseline" {
				events := cliCount(t, f.store, `SELECT count(*) FROM events`)
				refs, err := f.engine.ReadHostedObservations(t.Context(), f.project, attempt.ID)
				if err != nil {
					t.Fatal(err)
				}
				replayed, err := RetainHosted(t.Context(), captures, f.engine, attempt, final)
				after, readErr := f.engine.ReadHostedObservations(t.Context(), f.project, attempt.ID)
				if err != nil || readErr != nil || replayed != result || len(after) != len(refs) || cliCount(t, f.store, `SELECT count(*) FROM events`) != events {
					t.Fatal("held replay changed evidence/references/events", replayed, err, readErr)
				}
			}
		})
	}
}
