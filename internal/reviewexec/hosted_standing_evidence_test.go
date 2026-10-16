package reviewexec

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func standingEvidenceFixture(t *testing.T) (*attemptFixture, Attempt, ReviewEvidenceInput) {
	t.Helper()
	f, a, in := hostedPublicationFixture(t, true)
	start, err := time.Parse(time.RFC3339Nano, in.StartedAt)
	if err != nil {
		t.Fatal(err)
	}
	end, err := time.Parse(time.RFC3339Nano, in.FinishedAt)
	if err != nil {
		t.Fatal(err)
	}
	in.Hosted.PublicationID, in.Hosted.ProviderAttemptID = "", ""
	in.Hosted.ReviewIDs = []int64{}
	in.Hosted.StandingSummary = &HostedStandingSummaryMetadata{CommentID: 501, ProviderRunID: "33333333-3333-3333-3333-333333333333", UpdatedAt: start.Add(end.Sub(start) / 2).Format(time.RFC3339Nano)}
	in.Review.ReviewedFiles = []string{}
	return f, a, in
}

func TestHostedStandingEvidenceRetainsExactVariantAndPublicIdentity(t *testing.T) {
	f, a, in := standingEvidenceFixture(t)
	original := evidenceClone(in)
	beforeJournal := attemptState(t, f, a)
	beforeReceipts := attemptCount(t, f.store, `SELECT count(*) FROM receipts`)
	retained, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
	if err != nil || retained.State != "captured" || retained.Revision != 1 {
		t.Fatal("standing zero inventory not retained", retained, err)
	}
	before := adjudicationState(t, f, a)
	retry, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, evidenceClone(original))
	if err != nil || retry != retained || adjudicationState(t, f, a) != before {
		t.Fatal("standing exact replay changed effects", retry, err)
	}
	changed := evidenceClone(original)
	changed.Hosted.StandingSummary.CommentID++
	_, err = f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, changed)
	attemptCode(t, err, "review_conflict")
	if adjudicationState(t, f, a) != before {
		t.Fatal("standing changed replay affected source")
	}
	// Stored canonical bytes own both the nested pointer and all empty arrays.
	in.Hosted.StandingSummary.CommentID = 999
	in.Hosted.StandingSummary.ProviderRunID = "caller mutation"
	in.Hosted.ReviewIDs = append(in.Hosted.ReviewIDs, 999)
	value, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), retained.ID)
	if err != nil || !reflect.DeepEqual(value.Capture, original) || value.Anchor != *a.Anchor || value.AttemptID != a.ID || value.OperationID != a.OperationID {
		t.Fatal("standing retained identity changed", value, err)
	}
	if value.Capture.Hosted.ReviewIDs == nil || value.Capture.Hosted.Sources == nil || value.Capture.Review.Findings == nil || value.Capture.Review.ReviewedFiles == nil {
		t.Fatal("standing empty arrays became null")
	}
	response := publicEvidence(value)
	response.Capture.Hosted.StandingSummary.CommentID = 777
	if value.Capture.Hosted.StandingSummary.CommentID != 501 {
		t.Fatal("public response aliases retained metadata pointer")
	}
	path := "/api/v1/projects/" + attemptProject + "/reviews/evidence/" + retained.ID
	w := f.request(http.MethodGet, path, f.reader, "", "")
	public := evidenceHTTPRequire(t, w, http.StatusOK, "")
	capture := public["capture"].(map[string]any)
	h := capture["hosted"].(map[string]any)
	evidenceHTTPKeys(t, h, "publication_id", "provider_attempt_id", "trigger_comment_id", "review_ids", "observed_base_sha", "standing_summary")
	summary := h["standing_summary"].(map[string]any)
	evidenceHTTPKeys(t, summary, "comment_id", "provider_run_id", "updated_at")
	if public["channel"] != "hosted" || h["publication_id"] != "" || h["provider_attempt_id"] != "" || h["trigger_comment_id"] != float64(101) || !reflect.DeepEqual(h["review_ids"], []any{}) || summary["comment_id"] != float64(501) || summary["provider_run_id"] != original.Hosted.StandingSummary.ProviderRunID || summary["updated_at"] != original.Hosted.StandingSummary.UpdatedAt || !reflect.DeepEqual(capture["findings"], []any{}) || !reflect.DeepEqual(capture["reviewed_files"], []any{}) || capture["reported_findings"] != float64(0) || capture["outcome"] != "completed" || capture["complete"] != true {
		t.Fatal("public standing shape fabricated batch/source facts", public)
	}
	for _, private := range []string{"expected_anchor", "sources", "stdout", "stderr", "raw_body"} {
		if bytes.Contains(w.Body.Bytes(), []byte(`"`+private+`"`)) {
			t.Fatal("public response exposed private parser fields", private)
		}
	}
	if attemptState(t, f, a) != beforeJournal || attemptCount(t, f.store, `SELECT count(*) FROM receipts`) != beforeReceipts || adjudicationRead(t, f, retained.ID).Finalized {
		t.Fatal("capture/read/replay implicitly settled or finalized review")
	}
	if adjudicationState(t, f, a) != before {
		t.Fatal("standing reads changed durable source")
	}
}

func TestHostedStandingEvidenceZeroFinalizationIsExplicitAndCaptureImmutable(t *testing.T) {
	f, a, in := standingEvidenceFixture(t)
	in.Hosted.StandingSummary.UpdatedAt = in.FinishedAt // inclusive finish boundary
	in.Hosted.ObservedBaseSHA = in.ExpectedAnchor.MergeBaseSHA
	retained, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/projects/" + attemptProject + "/reviews/evidence/" + retained.ID
	original := append([]byte{}, f.request("GET", path, f.reader, "", "").Body.Bytes()...)
	evidenceHTTPRequire(t, f.request("POST", path+"/finalization", f.writer, "standing-before-settlement", `{"expected_revision":0}`), 409, "review_conflict")
	pull := projectionPull(2)
	if _, err := f.e.PublishEvidence(t.Context(), attemptProject, retained.ID, &pull); err != nil {
		t.Fatal("standing publication failed", err)
	}
	adjudicationComplete(t, f, a)
	first := evidenceHTTPReceipt(t, f.request("POST", path+"/finalization", f.writer, "standing-finalize", `{"expected_revision":0}`), retained.ID, "finalized", 1)
	before := adjudicationState(t, f, a)
	if w := f.request("POST", path+"/finalization", f.writer, "standing-finalize", `{"expected_revision":0}`); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), first) || adjudicationState(t, f, a) != before {
		t.Fatal("standing finalization replay changed effects")
	}
	state := adjudicationRead(t, f, retained.ID)
	if !state.Finalized || state.FrozenCounts == nil || state.FrozenCounts.Found != 0 || state.FrozenCounts.Accepted != 0 || state.FrozenCounts.Routed != 0 || len(state.Findings) != 0 {
		t.Fatal("standing zero inventory lost explicit frozen counts", state)
	}
	detail := cliPublicationDetail(t, f, 2, retained.ID)
	if detail.Run.Channel != "hosted" || !detail.Run.Finalized || detail.Run.Found != 0 || len(detail.Findings) != 0 {
		t.Fatal("standing native source fabricated batch findings", detail)
	}
	if current := f.request("GET", path, f.reader, "", ""); current.Code != 200 || !bytes.Equal(current.Body.Bytes(), original) {
		t.Fatal("finalization changed immutable capture")
	}
	replay, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, evidenceClone(in))
	if err != nil || replay != retained || adjudicationState(t, f, a) != before {
		t.Fatal("settled capture replay lost immutable receipt", replay, err)
	}
}

func TestHostedStandingEvidenceInvalidVariantsAreAtomic(t *testing.T) {
	cases := map[string]func(*ReviewEvidenceInput){
		"mixed_publication":      func(x *ReviewEvidenceInput) { x.Hosted.PublicationID = "11111111-1111-1111-1111-111111111111" },
		"mixed_provider_attempt": func(x *ReviewEvidenceInput) { x.Hosted.ProviderAttemptID = "22222222-2222-2222-2222-222222222222" },
		"mixed_reviews":          func(x *ReviewEvidenceInput) { x.Hosted.ReviewIDs = []int64{201} },
		"null_reviews":           func(x *ReviewEvidenceInput) { x.Hosted.ReviewIDs = nil },
		"null_sources":           func(x *ReviewEvidenceInput) { x.Hosted.Sources = nil },
		"mixed_sources":          func(x *ReviewEvidenceInput) { x.Hosted.Sources = []HostedFindingSource{{}} },
		"null_findings":          func(x *ReviewEvidenceInput) { x.Review.Findings = nil },
		"nonzero_findings": func(x *ReviewEvidenceInput) {
			x.Review.Findings = []reviewprovider.ParsedFinding{{Title: "Known source", Detail: "Literal source", Severity: "unknown", Path: "source.go"}}
			x.Review.ReportedFindings = 1
		},
		"nonzero_reported":   func(x *ReviewEvidenceInput) { x.Review.ReportedFindings = 1 },
		"null_files":         func(x *ReviewEvidenceInput) { x.Review.ReviewedFiles = nil },
		"invented_files":     func(x *ReviewEvidenceInput) { x.Review.ReviewedFiles = []string{"source.go"} },
		"not_complete":       func(x *ReviewEvidenceInput) { x.Review.Complete = false },
		"not_completed":      func(x *ReviewEvidenceInput) { x.Review.Outcome = "uncertain"; x.Review.Complete = false },
		"trigger_zero":       func(x *ReviewEvidenceInput) { x.Hosted.TriggerCommentID = 0 },
		"comment_zero":       func(x *ReviewEvidenceInput) { x.Hosted.StandingSummary.CommentID = 0 },
		"comment_negative":   func(x *ReviewEvidenceInput) { x.Hosted.StandingSummary.CommentID = -1 },
		"comment_is_trigger": func(x *ReviewEvidenceInput) { x.Hosted.StandingSummary.CommentID = x.Hosted.TriggerCommentID },
		"run_missing":        func(x *ReviewEvidenceInput) { x.Hosted.StandingSummary.ProviderRunID = "" },
		"run_malformed":      func(x *ReviewEvidenceInput) { x.Hosted.StandingSummary.ProviderRunID = "review_attempt_fake" },
		"run_uppercase": func(x *ReviewEvidenceInput) {
			x.Hosted.StandingSummary.ProviderRunID = "AAAAAAAA-3333-3333-3333-333333333333"
		},
		"time_missing":   func(x *ReviewEvidenceInput) { x.Hosted.StandingSummary.UpdatedAt = "" },
		"time_zero":      func(x *ReviewEvidenceInput) { x.Hosted.StandingSummary.UpdatedAt = "0001-01-01T00:00:00Z" },
		"time_malformed": func(x *ReviewEvidenceInput) { x.Hosted.StandingSummary.UpdatedAt = "yesterday" },
		"time_at_start":  func(x *ReviewEvidenceInput) { x.Hosted.StandingSummary.UpdatedAt = x.StartedAt },
		"time_before_start": func(x *ReviewEvidenceInput) {
			v, _ := time.Parse(time.RFC3339Nano, x.StartedAt)
			x.Hosted.StandingSummary.UpdatedAt = v.Add(-time.Nanosecond).Format(time.RFC3339Nano)
		},
		"time_after_finish": func(x *ReviewEvidenceInput) {
			v, _ := time.Parse(time.RFC3339Nano, x.FinishedAt)
			x.Hosted.StandingSummary.UpdatedAt = v.Add(time.Nanosecond).Format(time.RFC3339Nano)
		},
		"future": func(x *ReviewEvidenceInput) {
			x.Hosted.StandingSummary.UpdatedAt = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
		},
		"scope_mismatch":  func(x *ReviewEvidenceInput) { x.Hosted.ObservedBaseSHA = strings.Repeat("f", 40) },
		"missing_variant": func(x *ReviewEvidenceInput) { x.Hosted.StandingSummary = nil },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f, a, in := standingEvidenceFixture(t)
			change(&in)
			before := adjudicationState(t, f, a)
			_, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
			attemptCode(t, err, "validation_failed")
			if adjudicationState(t, f, a) != before {
				t.Fatal("invalid standing variant partially committed evidence/event/journal")
			}
		})
	}
	t.Run("foreign_posted_trigger", func(t *testing.T) {
		f, a, in := standingEvidenceFixture(t)
		in.Hosted.TriggerCommentID++
		before := adjudicationState(t, f, a)
		_, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
		attemptCode(t, err, "review_conflict")
		if adjudicationState(t, f, a) != before {
			t.Fatal("foreign trigger produced partial evidence")
		}
	})
}

func TestHostedStandingEvidenceStoredUnknownAndMalformedFieldsFailClosed(t *testing.T) {
	for _, mode := range []string{"unknown_summary", "summary_alias", "duplicate_summary_field", "null_summary", "null_comment", "missing_run", "null_run", "null_updated", "null_publication", "missing_publication", "unknown_hosted", "null_arrays", "nonzero_inventory", "outside_window"} {
		t.Run(mode, func(t *testing.T) {
			f, a, in := standingEvidenceFixture(t)
			retained, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
			if err != nil {
				t.Fatal(err)
			}
			var body string
			if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_evidence WHERE id=?`, retained.ID).Scan(&body); err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if json.Unmarshal([]byte(body), &record) != nil {
				t.Fatal("fixture record invalid")
			}
			capture := record["capture"].(map[string]any)
			hosted := capture["hosted"].(map[string]any)
			summary := hosted["standing_summary"].(map[string]any)
			switch mode {
			case "unknown_summary":
				summary["raw_body"] = "PRIVATE_PROVIDER_BODY"
			case "summary_alias":
				summary["providerRunId"] = summary["provider_run_id"]
				delete(summary, "provider_run_id")
			case "null_summary":
				hosted["standing_summary"] = nil
			case "null_comment":
				summary["comment_id"] = nil
			case "missing_run":
				delete(summary, "provider_run_id")
			case "null_run":
				summary["provider_run_id"] = nil
			case "null_updated":
				summary["updated_at"] = nil
			case "null_publication":
				hosted["publication_id"] = nil
			case "missing_publication":
				delete(hosted, "publication_id")
			case "unknown_hosted":
				hosted["body"] = "PRIVATE_PROVIDER_BODY"
			case "null_arrays":
				hosted["review_ids"] = nil
			case "nonzero_inventory":
				capture["review"].(map[string]any)["ReportedFindings"] = 1
			case "outside_window":
				summary["updated_at"] = in.StartedAt
			}
			bad := encoded(record)
			if mode == "duplicate_summary_field" {
				bad = strings.Replace(bad, `"comment_id":501`, `"comment_id":501,"comment_id":501`, 1)
			}
			if _, err := f.store.DB().Exec(`UPDATE review_execution_evidence SET body_json=? WHERE id=?`, bad, retained.ID); err != nil {
				t.Fatal(err)
			}
			before := adjudicationState(t, f, a)
			path := "/api/v1/projects/" + attemptProject + "/reviews/evidence/" + retained.ID
			evidenceHTTPRequire(t, f.request("GET", path, f.reader, "", ""), 500, "storage_unavailable")
			evidenceHTTPRequire(t, f.request("GET", path+"/adjudication", f.reader, "", ""), 500, "storage_unavailable")
			evidenceHTTPRequire(t, f.request("POST", path+"/finalization", f.writer, "corrupt-standing-finalize", `{"expected_revision":0}`), 500, "storage_unavailable")
			if adjudicationState(t, f, a) != before {
				t.Fatal("malformed retained metadata caused partial writes")
			}
		})
	}
}
