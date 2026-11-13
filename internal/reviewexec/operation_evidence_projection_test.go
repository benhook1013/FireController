package reviewexec

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFullOperationEvidenceProjectionMatchesCompactWithoutChangingJournal(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			var f *attemptFixture
			var a Attempt
			var in ReviewEvidenceInput
			if channel == "hosted" {
				f, a, in = standingEvidenceFixture(t)
			} else {
				f, a, in = evidenceFixture(t, "cli", true)
			}
			captured, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "controlled_completion", a.ExternalID); err != nil {
				t.Fatal(err)
			}
			before := evidenceState(t, f, a)
			scope := evidenceReadScope(t, f, attemptProject, f.reader)
			compact, err := f.e.ReadOperationStatus(t.Context(), scope, a.OperationID)
			if err != nil || compact.EvidenceID != captured.ID {
				t.Fatal(compact, err)
			}
			for _, expand := range []bool{false, true} {
				r := httptest.NewRequest("GET", "/api/v1/projects/"+attemptProject+"/reviews/operations/"+a.OperationID, nil)
				r.SetPathValue("project_id", attemptProject)
				r.SetPathValue("operation_id", a.OperationID)
				r.Header.Set("Authorization", "Bearer "+f.reader)
				if expand {
					r.Header.Set("X-FC-Operation-Evidence", "true")
				}
				w := httptest.NewRecorder()
				f.e.readOperationHTTP(w, r)
				var got Operation
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
					t.Fatal(w.Code, w.Body.String())
				}
				want := ""
				if expand {
					want = captured.ID
				}
				if got.EvidenceID != want || got.ID != compact.ID || got.Revision != compact.Revision || got.AttemptID != compact.AttemptID || got.State != compact.State {
					t.Fatal("operation/evidence binding changed", got)
				}
				if !strings.Contains(strings.Join(w.Header().Values("Vary"), ","), "X-FC-Operation-Evidence") {
					t.Fatal("missing response variation")
				}
			}
			if channel == "cli" {
				// Collect the existing completed request with its original key.
				// This must expose the same handle without creating another review.
				r := httptest.NewRequest("POST", "/api/v1/projects/"+attemptProject+"/reviews/requests", strings.NewReader(encoded(Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})))
				r.SetPathValue("project_id", attemptProject)
				r.Header.Set("Authorization", "Bearer "+f.writer)
				r.Header.Set("X-FC-Instance", f.store.InstanceID())
				r.Header.Set("Idempotency-Key", "capture-operation")
				r.Header.Set("X-FC-Operation-Evidence", "true")
				w := httptest.NewRecorder()
				f.e.enqueueHTTP(w, r)
				var got Operation
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.ID != a.OperationID || got.EvidenceID != captured.ID {
					t.Fatal("completed request lost its original evidence", w.Code, w.Body.String())
				}
			}
			if before != evidenceState(t, f, a) {
				t.Fatal("diagnostic projection wrote review state")
			}
			other := f.start(t, attemptProject, f.enqueue(t, attemptProject, "foreign-evidence-attempt", "cli", 4, 1))
			if _, err = f.store.DB().Exec(`UPDATE review_operations SET attempt_id=? WHERE id=?`, other.ID, a.OperationID); err != nil {
				t.Fatal(err)
			}
			if _, err = f.e.readOperationProjection(t.Context(), scope, a.OperationID, true); err == nil {
				t.Fatal("borrowed another operation's attempt")
			}
		})
	}
}
