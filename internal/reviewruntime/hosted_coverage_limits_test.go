package reviewruntime

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestRetainHostedReportedCoverageLimitsPreserveRawCaptureAndCompletion(t *testing.T) {
	for _, unsupported := range []bool{false, true} {
		name := "exact-files"
		if unsupported {
			name = "unverified-files"
		}
		t.Run(name, func(t *testing.T) {
			f, captures, attempt, record := hostedRetainFixture(t, false)
			const path = "design/project-management/implementation-tracking/player-access-and-session.md"
			count := "1"
			want := "CodeRabbit reported 1 file not fully reviewed because its changes were unavailable: " + path + "."
			if unsupported {
				count = "2"
				want = "CodeRabbit reported unavailable file changes, but its affected-file metadata could not be verified."
			}
			block := "<details><summary>Files not fully reviewed because their changes were unavailable: " + count + "</summary><dl><dd>\n\n* " + path + "\n\n</dd></dl></details>"
			body := record.Snapshot.Reviews[0].Body
			at := strings.Index(body, "</details>")
			if at < 0 {
				t.Fatal("fixture lacks Review info boundary")
			}
			record.Snapshot.Reviews[0].Body = body[:at] + block + "\n" + body[at:]
			before := hostedRetainClone(t, record)
			inventory, err := reviewprovider.ExtractHostedFindings(record.Snapshot, record.Intent, record.PostingActor, record.ProviderActor, record.EarliestPosting)
			if err != nil || !inventory.Complete || !reflect.DeepEqual(inventory.CoverageLimits, []string{want}) {
				t.Fatal("fixture lacks complete source with reported coverage limitation", inventory, err)
			}
			attemptBefore := cliAttemptState(t, f.store, attempt.ID)
			result, err := RetainHosted(t.Context(), captures, f.engine, attempt, record)
			if err != nil || !result.Complete || result.Reason != "capture_recorded" {
				t.Fatal("coverage metadata refused complete native inventory", result, err)
			}
			evidence, err := f.engine.ReadReviewEvidence(t.Context(), f.scopeWithAccess(t, f.reader, false, "read"), result.Receipt.ID)
			if err != nil || !reflect.DeepEqual(evidence.Capture.Review.CoverageLimits, []string{want}) || !evidence.Capture.Review.Complete || evidence.Capture.Review.ReportedFindings != 1 || len(evidence.Capture.Review.Findings) != 1 {
				t.Fatal("native evidence lost reported limits or changed counters/completion", evidence, err)
			}
			raw, err := captures.Read(t.Context(), attempt.ID, result.Capture)
			if err != nil || !reflect.DeepEqual(raw, before) || !reflect.DeepEqual(record, before) || cliAttemptState(t, f.store, attempt.ID) != attemptBefore {
				t.Fatal("derived coverage metadata changed raw capture or execution credit", err)
			}
			replay, err := RetainHosted(t.Context(), captures, f.engine, attempt, record)
			if err != nil || replay != result {
				t.Fatal("coverage metadata changed same-capture replay", replay, err)
			}
			compact, _ := json.Marshal(result)
			if strings.Contains(string(compact), want) || strings.Contains(string(compact), path) {
				t.Fatal("compact retention exposed private parser metadata")
			}
		})
	}
}
