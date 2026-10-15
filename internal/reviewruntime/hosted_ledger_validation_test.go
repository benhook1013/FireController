package reviewruntime

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestRetainHostedLedgerValidationLeavesInspectablePendingSource(t *testing.T) {
	for _, field := range []string{"title", "detail"} {
		t.Run(field, func(t *testing.T) {
			f, captures, attempt, valid := hostedRetainFixture(t, false)
			record := hostedRetainClone(t, valid)
			if field == "title" {
				record.Snapshot.ReviewComments[0].Body = hostedRetainInline(1, "Preserve exact\u0085 evidence.")
			} else {
				record.Snapshot.ReviewComments[0].Body = strings.Replace(record.Snapshot.ReviewComments[0].Body, "immutable detail", "immutable\u0085detail", 1)
			}
			inventory, err := reviewprovider.ExtractHostedFindings(record.Snapshot, record.Intent, record.PostingActor, record.ProviderActor, record.EarliestPosting)
			if err != nil || !inventory.Complete || inventory.Publication.State != "attributed" || len(inventory.Findings) != 1 || !strings.Contains(inventory.Findings[0].Detail, "\u0085") || field == "title" && !strings.Contains(inventory.Findings[0].Title, "\u0085") {
				t.Fatalf("fixture did not pass the real extractor intact: %+v %v", inventory, err)
			}
			before := cliAttemptState(t, f.store, attempt.ID)
			held, err := RetainHosted(t.Context(), captures, f.engine, attempt, record)
			if err != nil || held.Complete || held.Reason != "capture_invalid" || held.Receipt != (reviewexec.Receipt{}) || !validHostedReference(held.Capture) {
				t.Fatalf("ledger-invalid source must be an explicit retained hold: %+v %v", held, err)
			}
			loaded, err := captures.Read(t.Context(), attempt.ID, held.Capture)
			if err != nil || !reflect.DeepEqual(loaded, record) {
				t.Fatal("invalid source was lost or sanitized", err)
			}
			artifact := filepath.Join(captures.root, hostedCaptureFilename(attempt.ID, held.Capture))
			original, err := os.ReadFile(artifact)
			if err != nil || !bytes.Contains(original, []byte("\u0085")) {
				t.Fatal("literal C1 bytes were not retained", err)
			}
			observations, err := f.engine.ReadHostedObservations(t.Context(), f.project, attempt.ID)
			if err != nil || len(observations) != 1 || observations[0].Input.CaptureSHA256 != held.Capture.SHA256 {
				t.Fatal("invalid source has no durable inspectable reference", err)
			}
			events := cliCount(t, f.store, `SELECT count(*) FROM events`)
			if cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, attempt.ID) != 0 || cliAttemptState(t, f.store, attempt.ID) != before {
				t.Fatal("invalid source occupied final evidence or changed execution/reservation")
			}
			replay, err := RetainHosted(t.Context(), captures, f.engine, attempt, loaded)
			entries, readErr := os.ReadDir(captures.root)
			if err != nil || replay != held || readErr != nil || len(entries) != 1 || cliCount(t, f.store, `SELECT count(*) FROM events`) != events || cliCount(t, f.store, `SELECT count(*) FROM review_hosted_observations WHERE attempt_id=?`, attempt.ID) != 1 {
				t.Fatal("invalid source replay added artifacts, observations, events or a different result", replay, err, readErr)
			}
			// A genuinely later corrected provider observation can fill the still-free
			// final slot. Its predecessor remains inspectable with its original bytes.
			valid.Snapshot.ObservedAt = valid.Snapshot.ObservedAt.Add(time.Second)
			valid.FinishedAt = valid.FinishedAt.Add(time.Second)
			valid.Snapshot.ReviewComments[0].UpdatedAt = valid.Snapshot.ReviewComments[0].UpdatedAt.Add(time.Second)
			final, err := RetainHosted(t.Context(), captures, f.engine, attempt, valid)
			if err != nil || !final.Complete || final.Reason != "capture_recorded" || final.Receipt.ID == "" || final.Capture == held.Capture {
				t.Fatal("later valid observation could not occupy final slot", final, err)
			}
			evidence, err := f.engine.ReadReviewEvidence(t.Context(), f.scopeWithAccess(t, f.reader, false, "read"), final.Receipt.ID)
			if err != nil || len(evidence.Capture.Review.Findings) != 1 || evidence.Capture.Review.Findings[0].Detail != valid.Snapshot.ReviewComments[0].Body {
				t.Fatal("corrected final evidence does not retain its own exact source", err)
			}
			after, err := os.ReadFile(artifact)
			if err != nil || !bytes.Equal(original, after) || cliCount(t, f.store, `SELECT count(*) FROM review_hosted_observations WHERE attempt_id=?`, attempt.ID) != 2 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, attempt.ID) != 1 || cliAttemptState(t, f.store, attempt.ID) != before {
				t.Fatal("valid successor rewrote original source or changed settlement/reservation", err)
			}
			plan := reviewexec.SourcePlan{Repository: reviewprovider.Repository{ID: 901, FullName: "owner/repo", DefaultBranch: "main"}, Pull: reviewprovider.Pull{Number: 2, RepositoryID: 901, State: "open", HeadSHA: f.headSHA, BaseSHA: f.baseSHA, BaseRef: "main", ChangedFiles: 1}, ParentSHA: f.baseSHA, ObservedAt: attempt.Anchor.ObservedAt}
			covered, err := f.engine.Coverage(t.Context(), f.project, plan, "hosted")
			if err != nil || covered {
				t.Fatal("retention granted completed or finalized coverage", err)
			}
		})
	}
}

func TestRetainHostedLedgerConflictRemainsAnError(t *testing.T) {
	f, captures, attempt, record := hostedRetainFixture(t, false)
	first, err := RetainHosted(t.Context(), captures, f.engine, attempt, record)
	if err != nil || !first.Complete {
		t.Fatal("initial capture", first, err)
	}
	before := cliAttemptState(t, f.store, attempt.ID)
	record.Snapshot.ReviewComments[0].Body = hostedRetainInline(1, "A different final capture.")
	conflict, err := RetainHosted(t.Context(), captures, f.engine, attempt, record)
	assertCoreErrorCode(t, err, "review_conflict")
	if conflict.Complete || conflict.Reason != "evidence_unavailable" || conflict.Receipt != (reviewexec.Receipt{}) || !validHostedReference(conflict.Capture) || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, attempt.ID) != 1 || cliAttemptState(t, f.store, attempt.ID) != before {
		t.Fatal("conflicting evidence was masked or changed authoritative execution", conflict)
	}
}
