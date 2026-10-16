package client_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

func sampleStandingControlEvidence() client.ReviewEvidence {
	value := sampleHostedControlEvidence()
	value.Capture.Hosted.PublicationID, value.Capture.Hosted.ProviderAttemptID = "", ""
	value.Capture.Hosted.ReviewIDs = []int64{}
	value.Capture.Hosted.StandingSummary = &client.ReviewHostedStandingSummary{CommentID: 31, ProviderRunID: "12345678-1234-1234-1234-123456789abc", UpdatedAt: "2026-10-15T00:00:01Z"}
	value.Capture.StartedAt = "2026-10-15T00:00:00Z"
	value.Capture.FinishedAt = "2026-10-15T00:00:02Z"
	value.Capture.Findings = []client.ReviewEvidenceFinding{}
	value.Capture.ReportedFindings = 0
	return value
}

func standingControlJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertStandingControlInvalid(t *testing.T, data []byte) {
	t.Helper()
	got, err := readControlEvidenceWire(t, data)
	checkError(t, err, "invalid_response")
	if !reflect.DeepEqual(got, client.ReviewEvidence{}) {
		t.Fatal("invalid standing response returned partial source facts", got)
	}
}

func TestReviewStandingEvidenceSafePublicShapeAndTimeBoundaries(t *testing.T) {
	for _, mode := range []string{"parent", "merge_base", "finish_inclusive", "after_start_one_ns", "timestamp_offset", "maximum_hosted_capture", "existing_batch_without_standing"} {
		t.Run(mode, func(t *testing.T) {
			value := sampleStandingControlEvidence()
			switch mode {
			case "merge_base":
				value.Capture.Hosted.ObservedBaseSHA = value.Anchor.MergeBaseSHA
			case "finish_inclusive":
				value.Capture.Hosted.StandingSummary.UpdatedAt = value.Capture.FinishedAt
			case "after_start_one_ns":
				value.Capture.Hosted.StandingSummary.UpdatedAt = "2026-10-15T00:00:00.000000001Z"
			case "timestamp_offset":
				value.Capture.Hosted.StandingSummary.UpdatedAt = "2026-10-15T01:00:01+01:00"
			case "maximum_hosted_capture":
				value.Capture.CaptureBytes = 48 << 20
			case "existing_batch_without_standing":
				value = sampleHostedControlEvidence()
			}
			data := standingControlJSON(t, value)
			got, err := readControlEvidenceWire(t, data)
			if err != nil || !reflect.DeepEqual(got, value) {
				t.Fatal("safe public variant changed identity, scope, times or arrays", got, err)
			}
			if mode == "existing_batch_without_standing" {
				return
			}
			if got.Capture.Findings == nil || got.Capture.ReviewedFiles == nil || got.Capture.Hosted.ReviewIDs == nil || got.Capture.ReportedFindings != 0 || !got.Capture.Complete || got.Capture.Outcome != "completed" || got.Capture.Hosted.PublicationID != "" || got.Capture.Hosted.ProviderAttemptID != "" {
				t.Fatal("standing summary invented source inventory or batch identities", got)
			}
			var object map[string]any
			if err := json.Unmarshal(data, &object); err != nil {
				t.Fatal(err)
			}
			hosted := object["capture"].(map[string]any)["hosted"].(map[string]any)
			if len(hosted) != 6 || len(hosted["standing_summary"].(map[string]any)) != 3 {
				t.Fatal("standing safe shape gained extra fields", hosted)
			}
			for _, key := range []string{"sources", "expected_anchor", "stdout", "stderr", "raw_body", "provider_path"} {
				if strings.Contains(string(data), `"`+key+`"`) {
					t.Fatal("public metadata leaked private fields", key)
				}
			}
		})
	}
}

func TestReviewStandingEvidenceRejectsMixedInventoryScopeAndTimes(t *testing.T) {
	cases := map[string]func(*client.ReviewEvidence){
		"mixed_publication": func(v *client.ReviewEvidence) {
			v.Capture.Hosted.PublicationID = "12345678-1234-1234-1234-123456789abc"
		},
		"mixed_provider_attempt": func(v *client.ReviewEvidence) {
			v.Capture.Hosted.ProviderAttemptID = "abcdefab-abcd-abcd-abcd-abcdefabcdef"
		},
		"mixed_reviews": func(v *client.ReviewEvidence) { v.Capture.Hosted.ReviewIDs = []int64{11} },
		"null_reviews":  func(v *client.ReviewEvidence) { v.Capture.Hosted.ReviewIDs = nil },
		"null_findings": func(v *client.ReviewEvidence) { v.Capture.Findings = nil },
		"nonzero_findings": func(v *client.ReviewEvidence) {
			v.Capture.Findings = sampleHostedControlEvidence().Capture.Findings[:1]
			v.Capture.ReportedFindings = 1
		},
		"nonzero_reported": func(v *client.ReviewEvidence) { v.Capture.ReportedFindings = 1 },
		"null_files":       func(v *client.ReviewEvidence) { v.Capture.ReviewedFiles = nil },
		"invented_files":   func(v *client.ReviewEvidence) { v.Capture.ReviewedFiles = []string{"source.go"} },
		"incomplete":       func(v *client.ReviewEvidence) { v.Capture.Complete = false },
		"failed":           func(v *client.ReviewEvidence) { v.Capture.Outcome, v.Capture.Complete = "failed", false },
		"missing_variant":  func(v *client.ReviewEvidence) { v.Capture.Hosted.StandingSummary = nil },
		"CLI_variant":      func(v *client.ReviewEvidence) { v.Channel = "cli"; v.Capture.ReviewedFiles = []string{"source.go"} },
		"trigger_zero":     func(v *client.ReviewEvidence) { v.Capture.Hosted.TriggerCommentID = 0 },
		"comment_zero":     func(v *client.ReviewEvidence) { v.Capture.Hosted.StandingSummary.CommentID = 0 },
		"comment_negative": func(v *client.ReviewEvidence) { v.Capture.Hosted.StandingSummary.CommentID = -1 },
		"comment_equals_trigger": func(v *client.ReviewEvidence) {
			v.Capture.Hosted.StandingSummary.CommentID = v.Capture.Hosted.TriggerCommentID
		},
		"run_empty":     func(v *client.ReviewEvidence) { v.Capture.Hosted.StandingSummary.ProviderRunID = "" },
		"run_malformed": func(v *client.ReviewEvidence) { v.Capture.Hosted.StandingSummary.ProviderRunID = "review_attempt_fake" },
		"run_uppercase": func(v *client.ReviewEvidence) {
			v.Capture.Hosted.StandingSummary.ProviderRunID = strings.ToUpper(v.Capture.Hosted.StandingSummary.ProviderRunID)
		},
		"scope_empty":     func(v *client.ReviewEvidence) { v.Capture.Hosted.ObservedBaseSHA = "" },
		"scope_other":     func(v *client.ReviewEvidence) { v.Capture.Hosted.ObservedBaseSHA = strings.Repeat("d", 40) },
		"scope_candidate": func(v *client.ReviewEvidence) { v.Capture.Hosted.ObservedBaseSHA = v.Anchor.CandidateSHA },
		"scope_uppercase": func(v *client.ReviewEvidence) {
			v.Capture.Hosted.ObservedBaseSHA = strings.ToUpper(v.Capture.Hosted.ObservedBaseSHA)
		},
		"time_empty":    func(v *client.ReviewEvidence) { v.Capture.Hosted.StandingSummary.UpdatedAt = "" },
		"time_zero":     func(v *client.ReviewEvidence) { v.Capture.Hosted.StandingSummary.UpdatedAt = "0001-01-01T00:00:00Z" },
		"time_invalid":  func(v *client.ReviewEvidence) { v.Capture.Hosted.StandingSummary.UpdatedAt = "tomorrow" },
		"time_at_start": func(v *client.ReviewEvidence) { v.Capture.Hosted.StandingSummary.UpdatedAt = v.Capture.StartedAt },
		"time_before_start": func(v *client.ReviewEvidence) {
			v.Capture.Hosted.StandingSummary.UpdatedAt = "2026-10-14T23:59:59.999999999Z"
		},
		"time_after_finish": func(v *client.ReviewEvidence) {
			v.Capture.Hosted.StandingSummary.UpdatedAt = "2026-10-15T00:00:02.000000001Z"
		},
		"reversed_capture": func(v *client.ReviewEvidence) { v.Capture.FinishedAt = "2026-10-14T00:00:00Z" },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			value := sampleStandingControlEvidence()
			edit(&value)
			assertStandingControlInvalid(t, standingControlJSON(t, value))
		})
	}
}

func TestReviewStandingEvidenceStrictRequiredSafeFields(t *testing.T) {
	valid := standingControlJSON(t, sampleStandingControlEvidence())
	for _, target := range []string{"hosted", "standing"} {
		fields := []string{"publication_id", "provider_attempt_id", "trigger_comment_id", "review_ids", "observed_base_sha", "standing_summary"}
		if target == "standing" {
			fields = []string{"comment_id", "provider_run_id", "updated_at"}
		}
		for _, field := range fields {
			for _, mode := range []string{"missing", "null", "alias", "extra", "duplicate", "wrongtype"} {
				t.Run(target+"/"+field+"/"+mode, func(t *testing.T) {
					var object map[string]any
					if err := json.Unmarshal(valid, &object); err != nil {
						t.Fatal(err)
					}
					hosted := object["capture"].(map[string]any)["hosted"].(map[string]any)
					inner := hosted
					if target == "standing" {
						inner = hosted["standing_summary"].(map[string]any)
					}
					old := inner[field]
					switch mode {
					case "missing":
						delete(inner, field)
					case "null":
						inner[field] = nil
					case "alias":
						inner[strings.ToUpper(field)] = old
						delete(inner, field)
					case "extra":
						inner["raw_body"] = "PRIVATE_PROVIDER_TEXT"
					case "wrongtype":
						inner[field] = false
					}
					data := standingControlJSON(t, object)
					if mode == "duplicate" {
						key, _ := json.Marshal(field)
						value, _ := json.Marshal(old)
						needle := string(key) + ":" + string(value)
						data = []byte(strings.Replace(string(data), needle, needle+","+needle, 1))
					}
					assertStandingControlInvalid(t, data)
				})
			}
		}
	}
	for _, literal := range []string{`1.5`, `"31"`, `9223372036854775808`, `"\ud800"`} {
		data := []byte(strings.Replace(string(valid), `"comment_id":31`, `"comment_id":`+literal, 1))
		assertStandingControlInvalid(t, data)
	}
	// A byte outside valid UTF-8 must not be repaired into apparently safe data.
	data := []byte(strings.Replace(string(valid), `"updated_at":"2026-10-15T00:00:01Z"`, "\"updated_at\":\"\xff\"", 1))
	assertStandingControlInvalid(t, data)
}

func TestReviewStandingEvidenceTimesCompareInstantsWithoutInventedRunIdentity(t *testing.T) {
	value := sampleStandingControlEvidence()
	// Provider UUID identity is independent of a local operation or attempt ID.
	// Reuse an otherwise valid historical batch UUID without deriving a new one.
	value.Capture.Hosted.StandingSummary.ProviderRunID = sampleHostedControlEvidence().Capture.Hosted.ProviderAttemptID
	start, _ := time.Parse(time.RFC3339Nano, value.Capture.StartedAt)
	value.Capture.Hosted.StandingSummary.UpdatedAt = start.Add(time.Nanosecond).In(time.FixedZone("offset", 3600)).Format(time.RFC3339Nano)
	got, err := readControlEvidenceWire(t, standingControlJSON(t, value))
	if err != nil || !reflect.DeepEqual(got, value) {
		t.Fatal("client invented UUID relation or compared timestamp text", got, err)
	}
}
