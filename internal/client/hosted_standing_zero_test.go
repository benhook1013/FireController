package client_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func standingZeroControlEvidence() client.ReviewEvidence {
	value := sampleHostedControlEvidence()
	value.Capture.Hosted.PublicationID, value.Capture.Hosted.ProviderAttemptID, value.Capture.Hosted.ReviewIDs = "", "", nil
	value.Capture.Hosted.StandingZero = &client.ReviewHostedStandingZero{BaselineCaptureID: "review_capture_" + strings.Repeat("2", 32), BaselineCaptureSHA256: strings.Repeat("2", 64), CommentID: 31, CompletionCommentID: 32, RunID: "33333333-3333-3333-3333-333333333333"}
	value.Capture.Findings = []client.ReviewEvidenceFinding{}
	value.Capture.ReportedFindings = 0
	return value
}

func TestHostedStandingZeroClientSafeHTTPVariant(t *testing.T) {
	value := standingZeroControlEvidence()
	for _, base := range []string{value.Anchor.ParentSHA, value.Anchor.MergeBaseSHA} {
		value.Capture.Hosted.ObservedBaseSHA = base
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		got, err := readControlEvidenceWire(t, data)
		if err != nil || got.Capture.Hosted.StandingZero == nil || *got.Capture.Hosted.StandingZero != *value.Capture.Hosted.StandingZero || got.Capture.Findings == nil || len(got.Capture.Findings) != 0 || got.Capture.ReviewedFiles == nil || got.Capture.ReportedFindings != 0 {
			t.Fatal(got, err)
		}
		for _, invented := range []string{"publication_id", "provider_attempt_id", "review_ids"} {
			if strings.Contains(string(data), invented) {
				t.Fatal("invented batch identity", invented)
			}
		}
	}
	// Existing marked positive and marked zero responses retain their old shape.
	old := sampleHostedControlEvidence()
	for _, zero := range []bool{false, true} {
		if zero {
			old.Capture.Findings = []client.ReviewEvidenceFinding{}
			old.Capture.ReportedFindings = 0
		}
		data, _ := json.Marshal(old)
		got, err := readControlEvidenceWire(t, data)
		if err != nil || got.Capture.Hosted.StandingZero != nil || got.Capture.Hosted.PublicationID != old.Capture.Hosted.PublicationID {
			t.Fatal("marked source read regressed", err)
		}
	}
}

func TestHostedStandingZeroClientRejectsMixedIncompleteAndForeignFacts(t *testing.T) {
	changes := map[string]func(*client.ReviewEvidence){
		"baseline absent": func(v *client.ReviewEvidence) { v.Capture.Hosted.StandingZero.BaselineCaptureID = "" },
		"baseline hash mismatch": func(v *client.ReviewEvidence) {
			v.Capture.Hosted.StandingZero.BaselineCaptureSHA256 = strings.Repeat("3", 64)
		},
		"current capture baseline": func(v *client.ReviewEvidence) {
			v.Capture.Hosted.StandingZero.BaselineCaptureID = v.Capture.CaptureID
			v.Capture.Hosted.StandingZero.BaselineCaptureSHA256 = v.Capture.CaptureSHA256
		},
		"mixed publication": func(v *client.ReviewEvidence) {
			v.Capture.Hosted.PublicationID = "11111111-1111-1111-1111-111111111111"
		},
		"mixed provider attempt": func(v *client.ReviewEvidence) {
			v.Capture.Hosted.ProviderAttemptID = "22222222-2222-2222-2222-222222222222"
		},
		"invented review":        func(v *client.ReviewEvidence) { v.Capture.Hosted.ReviewIDs = []int64{31} },
		"missing standing ID":    func(v *client.ReviewEvidence) { v.Capture.Hosted.StandingZero.CommentID = 0 },
		"negative completion ID": func(v *client.ReviewEvidence) { v.Capture.Hosted.StandingZero.CompletionCommentID = -1 },
		"duplicate IDs":          func(v *client.ReviewEvidence) { v.Capture.Hosted.StandingZero.CompletionCommentID = 31 },
		"standing equals trigger": func(v *client.ReviewEvidence) {
			v.Capture.Hosted.StandingZero.CommentID = v.Capture.Hosted.TriggerCommentID
		},
		"finish equals trigger": func(v *client.ReviewEvidence) {
			v.Capture.Hosted.StandingZero.CompletionCommentID = v.Capture.Hosted.TriggerCommentID
		},
		"noncanonical run": func(v *client.ReviewEvidence) {
			v.Capture.Hosted.StandingZero.RunID = "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA"
		},
		"null findings": func(v *client.ReviewEvidence) { v.Capture.Findings = nil },
		"null files":    func(v *client.ReviewEvidence) { v.Capture.ReviewedFiles = nil },
		"fake files":    func(v *client.ReviewEvidence) { v.Capture.ReviewedFiles = []string{"file.go"} },
		"fake count":    func(v *client.ReviewEvidence) { v.Capture.ReportedFindings = 1 },
		"findings": func(v *client.ReviewEvidence) {
			v.Capture.Findings = sampleHostedControlEvidence().Capture.Findings
			v.Capture.ReportedFindings = 3
		},
		"incomplete":    func(v *client.ReviewEvidence) { v.Capture.Complete = false; v.Capture.Outcome = "uncertain" },
		"foreign base":  func(v *client.ReviewEvidence) { v.Capture.Hosted.ObservedBaseSHA = strings.Repeat("f", 40) },
		"reversed time": func(v *client.ReviewEvidence) { v.Capture.FinishedAt = "2000-01-01T00:00:00Z" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			value := standingZeroControlEvidence()
			change(&value)
			data, _ := json.Marshal(value)
			_, err := readControlEvidenceWire(t, data)
			checkError(t, err, "invalid_response")
		})
	}
}

func TestHostedStandingZeroClientStrictWireFields(t *testing.T) {
	valid, _ := json.Marshal(standingZeroControlEvidence())
	for _, body := range []string{
		strings.Replace(string(valid), `"comment_id":31`, `"comment_id":null`, 1),
		strings.Replace(string(valid), `"comment_id":31,`, "", 1),
		strings.Replace(string(valid), `"comment_id":31`, `"comment_id":31.0`, 1),
		strings.Replace(string(valid), `"comment_id":31`, `"CommentID":31`, 1),
		strings.Replace(string(valid), `"comment_id":31`, `"comment_id":31,"unknown":true`, 1),
		strings.Replace(string(valid), `"comment_id":31`, `"comment_id":31,"comment_id":31`, 1),
		func() string {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(valid, &fields)
			var capture map[string]json.RawMessage
			_ = json.Unmarshal(fields["capture"], &capture)
			var hosted map[string]json.RawMessage
			_ = json.Unmarshal(capture["hosted"], &hosted)
			hosted["standing_zero"] = json.RawMessage("null")
			capture["hosted"], _ = json.Marshal(hosted)
			fields["capture"], _ = json.Marshal(capture)
			data, _ := json.Marshal(fields)
			return string(data)
		}(),
		strings.Replace(string(valid), `"standing_zero":{`, `"publication_id":"","standing_zero":{`, 1),
		strings.Replace(string(valid), `"standing_zero":{`, `"review_ids":[],"standing_zero":{`, 1),
		strings.Replace(string(valid), `"run_id":"33333333-3333-3333-3333-333333333333"`, `"run_id":null`, 1),
		strings.Replace(string(valid), `"run_id":"33333333-3333-3333-3333-333333333333"`, `"run_id":"\ud800"`, 1),
	} {
		_, err := readControlEvidenceWire(t, []byte(body))
		checkError(t, err, "invalid_response")
	}
}
