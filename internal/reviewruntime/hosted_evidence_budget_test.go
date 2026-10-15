package reviewruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestRetainHostedCanonicalInventoryBudgetPreservesRawSource(t *testing.T) {
	for _, test := range []struct {
		name   string
		filler string
		large  bool
	}{
		{"HTML escaping exceeds ledger budget", "&", true},
		{"same raw size under canonical budget", "a", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, captures, attempt, record := hostedRetainFixture(t, false)
			const count = 100
			root := record.Snapshot.ReviewComments[0]
			record.Snapshot.ReviewComments = make([]reviewprovider.ReviewComment, 0, count)
			for i := range count {
				comment := root
				comment.ID += int64(i)
				comment.Body = "_⚠️ Potential issue_ | _🟠 Major_\n\n" + fmt.Sprintf("**Preserve literal source %03d.**\n\n", i+1) + strings.Repeat(test.filler, 12<<10) + "\n\n" + fmt.Sprintf("<!-- cr-comment:v1:%024x -->", i+1)
				record.Snapshot.ReviewComments = append(record.Snapshot.ReviewComments, comment)
			}
			record.Snapshot.Reviews[0].Body = strings.Replace(record.Snapshot.Reviews[0].Body, "posted: 1", "posted: 100", 1)
			inventory, err := reviewprovider.ExtractHostedFindings(record.Snapshot, record.Intent, record.PostingActor, record.ProviderActor, record.EarliestPosting)
			if err != nil || !inventory.Complete || inventory.Publication.State != "attributed" || len(inventory.Findings) != count {
				t.Fatalf("fixture did not pass real atomic extraction: complete=%v reason=%s count=%d err=%v", inventory.Complete, inventory.Reason, len(inventory.Findings), err)
			}
			// Encoding the detail strings alone is a lower bound on the actual
			// evidence input. Their source bytes fit; escaping can exceed its cap.
			details := make([]string, 0, count)
			plainBytes := 0
			for i, finding := range inventory.Findings {
				if len(finding.Detail) > 16<<10 || finding.Detail != record.Snapshot.ReviewComments[i].Body {
					t.Fatal("fixture exceeds atomic limit or extractor changed source")
				}
				plainBytes += len(finding.Detail)
				details = append(details, finding.Detail)
			}
			encodedDetails, err := json.Marshal(details)
			if err != nil || plainBytes >= reviewexec.MaxEvidenceInputBytes || (len(encodedDetails) > reviewexec.MaxEvidenceInputBytes) != test.large {
				t.Fatalf("fixture does not isolate canonical escaping budget: raw=%d encoded=%d err=%v", plainBytes, len(encodedDetails), err)
			}
			before := cliAttemptState(t, f.store, attempt.ID)
			result, err := RetainHosted(t.Context(), captures, f.engine, attempt, record)
			if test.large {
				if err != nil || result.Complete || result.Reason != "finding_inventory_limit" || result.Receipt != (reviewexec.Receipt{}) || !validHostedReference(result.Capture) {
					t.Fatalf("oversize source must remain inspectable without final credit: %+v err=%v", result, err)
				}
			} else if err != nil || !result.Complete || result.Reason != "capture_recorded" || result.Receipt.ID == "" {
				t.Fatalf("under-budget complete source refused: %+v err=%v", result, err)
			}
			loaded, err := captures.Read(t.Context(), attempt.ID, result.Capture)
			if err != nil || !reflect.DeepEqual(loaded, record) {
				t.Fatal("full source was truncated or changed", err)
			}
			artifact := filepath.Join(captures.root, hostedCaptureFilename(attempt.ID, result.Capture))
			originalBytes, err := os.ReadFile(artifact)
			if err != nil || !bytes.Contains(originalBytes, []byte(strings.Repeat(test.filler, 12<<10))) {
				t.Fatal("raw artifact discarded literal body", err)
			}
			observations, err := f.engine.ReadHostedObservations(t.Context(), f.project, attempt.ID)
			if err != nil || len(observations) != 1 || observations[0].Input.CaptureID != result.Capture.ID || observations[0].Input.CaptureSHA256 != result.Capture.SHA256 || observations[0].Input.CaptureBytes != result.Capture.Bytes {
				t.Fatal("durable reference to original source missing", err)
			}
			eventCount := cliCount(t, f.store, `SELECT count(*) FROM events`)
			wantEvidence := 1
			if test.large {
				wantEvidence = 0
			}
			if cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, attempt.ID) != wantEvidence || cliCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.captured'`) != wantEvidence || cliAttemptState(t, f.store, attempt.ID) != before {
				t.Fatal("retention changed reservation/settlement or granted incorrect final evidence")
			}
			if !test.large {
				evidence, err := f.engine.ReadReviewEvidence(t.Context(), f.scopeWithAccess(t, f.reader, false, "read"), result.Receipt.ID)
				if err != nil || !evidence.Capture.Review.Complete || len(evidence.Capture.Review.Findings) != count || evidence.Capture.Review.ReportedFindings != count {
					t.Fatal("normal complete inventory lost counts", err)
				}
				for i, finding := range evidence.Capture.Review.Findings {
					if finding.Detail != record.Snapshot.ReviewComments[i].Body {
						t.Fatal("under-budget ledger changed finding text")
					}
				}
			}
			plan := reviewexec.SourcePlan{Repository: reviewprovider.Repository{ID: 901, FullName: "owner/repo", DefaultBranch: "main"}, Pull: reviewprovider.Pull{Number: 2, RepositoryID: 901, State: "open", HeadSHA: f.headSHA, BaseSHA: f.baseSHA, BaseRef: "main", ChangedFiles: 1}, ParentSHA: f.baseSHA, ObservedAt: attempt.Anchor.ObservedAt}
			covered, err := f.engine.Coverage(t.Context(), f.project, plan, "hosted")
			if err != nil || covered {
				t.Fatal("retention granted completed/finalized comparison credit", err)
			}
			replay, err := RetainHosted(t.Context(), captures, f.engine, attempt, loaded)
			if err != nil || replay != result || cliCount(t, f.store, `SELECT count(*) FROM events`) != eventCount || cliCount(t, f.store, `SELECT count(*) FROM review_hosted_observations WHERE attempt_id=?`, attempt.ID) != 1 || cliAttemptState(t, f.store, attempt.ID) != before {
				t.Fatal("local retry duplicated events, observations or attempt transitions", err)
			}
			afterBytes, err := os.ReadFile(artifact)
			if err != nil || !bytes.Equal(originalBytes, afterBytes) {
				t.Fatal("retry replaced retained source bytes", err)
			}
			entries, err := os.ReadDir(captures.root)
			if err != nil || len(entries) != 1 {
				t.Fatal("retry added extra artifacts", err)
			}
		})
	}
}

func TestRetainHostedEvidenceBudgetDoesNotMaskStorageFailure(t *testing.T) {
	f, captures, attempt, record := hostedRetainFixture(t, false)
	before := cliAttemptState(t, f.store, attempt.ID)
	// Observation retention remains available while final evidence persistence
	// fails. Only actual encoded oversize may become a nonerror inventory hold.
	if _, err := f.store.DB().Exec(`CREATE TRIGGER evidence_budget_storage_fault BEFORE INSERT ON review_execution_evidence BEGIN SELECT RAISE(ABORT,'synthetic evidence fault'); END`); err != nil {
		t.Fatal(err)
	}
	result, err := RetainHosted(t.Context(), captures, f.engine, attempt, record)
	assertCoreErrorCode(t, err, "storage_unavailable")
	if result.Complete || result.Reason != "evidence_unavailable" || result.Receipt.ID != "" || !validHostedReference(result.Capture) {
		t.Fatalf("storage failure became a successful inventory hold: %+v", result)
	}
	retained, err := captures.Read(t.Context(), attempt.ID, result.Capture)
	if err != nil || !reflect.DeepEqual(retained, record) {
		t.Fatal("persistence failure discarded retained raw source", err)
	}
	observations, err := f.engine.ReadHostedObservations(t.Context(), f.project, attempt.ID)
	if err != nil || len(observations) != 1 || observations[0].Input.CaptureID != result.Capture.ID {
		t.Fatal("persistence failure lost durable source reference", err)
	}
	if cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, attempt.ID) != 0 || cliCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.captured'`) != 0 || cliAttemptState(t, f.store, attempt.ID) != before {
		t.Fatal("storage failure added final credit or changed attempt reservation")
	}
}
