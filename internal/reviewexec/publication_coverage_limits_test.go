package reviewexec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPublishEvidenceBoundsRetainedCoverageDisplayWithoutChangingSource(t *testing.T) {
	for _, tc := range []struct {
		name       string
		limits     []string
		incomplete bool
		omitted    bool
	}{
		{"complete-sixteen", strings.Fields(strings.Repeat("limitation ", 16)), false, false},
		{"utf8-byte-boundary", []string{strings.Repeat("é", 512)}, false, false},
		{"incomplete-fifteen", strings.Fields(strings.Repeat("limitation ", 15)), true, false},
		{"historical-seventeen", strings.Fields(strings.Repeat("limitation ", 17)), false, true},
		{"historical-long", []string{strings.Repeat("x", 1025)}, false, true},
		{"historical-long-utf8", []string{strings.Repeat("é", 513)}, false, true},
		{"historical-incomplete-sixteen", strings.Fields(strings.Repeat("limitation ", 16)), true, true},
		{"historical-blank", []string{" \t\n"}, false, true},
		{"historical-control", []string{"retained\x00source"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome := "completed"
			if tc.incomplete {
				outcome = "failed"
			}
			f, a, evidence := cliPublicationFixture(t, 1, outcome)
			evidence.Capture.Review.CoverageLimits = append([]string{}, tc.limits...)
			if (validateNewCoverageLimits(evidence.Capture) != nil) != tc.omitted {
				t.Fatal("fixture changed current warning admission")
			}
			// Retain an originally admitted capture/fingerprint rather than weaken
			// current ingestion to manufacture an out-of-policy source.
			canonical, err := json.Marshal(evidence.Capture)
			if err != nil || ValidateRetainedReviewCapture(evidence.Capture) != nil {
				t.Fatal("invalid retained fixture", err)
			}
			digest := sha256.Sum256(canonical)
			body, fingerprint := encoded(evidence), hex.EncodeToString(digest[:])
			if _, err = f.store.DB().Exec(`UPDATE review_execution_evidence SET body_json=?,fingerprint=? WHERE id=?`, body, fingerprint, evidence.ID); err != nil {
				t.Fatal(err)
			}
			attemptBefore := attemptState(t, f, a)
			first, err := f.e.PublishEvidence(t.Context(), attemptProject, evidence.ID, cliPublicationPull(2))
			if err != nil {
				t.Fatal("retained warning metadata blocked publication", err)
			}
			detail := cliPublicationDetail(t, f, 2, evidence.ID)
			want := append([]string{}, tc.limits...)
			if tc.omitted {
				want = []string{"Source coverage limitations were omitted from this display; see the original review evidence."}
			}
			if tc.incomplete {
				want = append(want, "Incomplete CLI capture; retained findings do not establish completed review coverage.")
			}
			if !reflect.DeepEqual(detail.Run.CoverageLimits, want) || detail.Run.Complete == tc.incomplete || detail.Run.Attributable == tc.incomplete || detail.Run.Finalized || detail.Run.Accepted != 0 || detail.Run.Routed != 0 || len(detail.Findings) != 1 {
				t.Fatal("bounded display changed source counts, completion or omissions", detail)
			}
			if len(detail.Run.CoverageLimits) > 16 {
				t.Fatal("display warning count exceeds its contract")
			}
			for _, limit := range detail.Run.CoverageLimits {
				if len(limit) > 1024 || !utf8.ValidString(limit) {
					t.Fatal("display warning is not bounded UTF-8")
				}
			}
			state := evidenceState(t, f, a)
			second, err := f.e.PublishEvidence(t.Context(), attemptProject, evidence.ID, nil)
			if err != nil || second != first || evidenceState(t, f, a) != state || attemptState(t, f, a) != attemptBefore {
				t.Fatal("same-source publication replay changed execution or emitted effects", second, err)
			}
			retained, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), evidence.ID)
			if err != nil || !reflect.DeepEqual(retained.Capture, evidence.Capture) {
				t.Fatal("display normalization changed original readable evidence", err)
			}
			var retainedBody, retainedFingerprint string
			if err = f.store.DB().QueryRow(`SELECT body_json,fingerprint FROM review_execution_evidence WHERE id=?`, evidence.ID).Scan(&retainedBody, &retainedFingerprint); err != nil || retainedBody != body || retainedFingerprint != fingerprint {
				t.Fatal("publication rewrote immutable source metadata or fingerprint", err)
			}
		})
	}
}
