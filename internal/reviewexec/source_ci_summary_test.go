package reviewexec

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestSummarizeSourceCIUniformUnprovenReasonPreservesFacts(t *testing.T) {
	tests := []struct {
		name       string
		state      string
		reason     string
		unproven   []reviewprovider.RequiredCIAssessmentCheck
		wantReason string
	}{
		{"uniform", "unproven", "ci_required_checks_unproven", []reviewprovider.RequiredCIAssessmentCheck{{State: "unproven", Reason: "ci_status_origin_unproven"}, {State: "unproven", Reason: "ci_status_origin_unproven"}}, "ci_status_origin_unproven"},
		{"mixed", "unproven", "ci_required_checks_unproven", []reviewprovider.RequiredCIAssessmentCheck{{State: "unproven", Reason: "ci_status_origin_unproven"}, {State: "unproven", Reason: "ci_attempt_unproven"}}, "ci_required_checks_unproven"},
		{"missing_reason", "unproven", "ci_required_checks_unproven", []reviewprovider.RequiredCIAssessmentCheck{{State: "unproven", Reason: "ci_status_origin_unproven"}, {State: "unproven"}}, "ci_required_checks_unproven"},
		{"no_unproven", "unproven", "ci_required_checks_unproven", nil, "ci_required_checks_unproven"},
		{"other_reason", "unproven", "ci_source_unavailable", []reviewprovider.RequiredCIAssessmentCheck{{State: "unproven", Reason: "ci_status_origin_unproven"}}, "ci_source_unavailable"},
		{"other_state", "pending", "ci_required_checks_pending", []reviewprovider.RequiredCIAssessmentCheck{{State: "unproven", Reason: "ci_status_origin_unproven"}}, "ci_required_checks_pending"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			observed := time.Date(2026, time.February, 3, 4, 5, 6, 7, time.UTC)
			checks := []reviewprovider.RequiredCIAssessmentCheck{{State: "satisfied", CheckRunID: 100}, {State: "failed", Reason: "ci_check_failed", CheckRunID: 101}, {State: "pending", Reason: "ci_check_pending", CheckRunID: 102}}
			checks = append(checks, tt.unproven...)
			o := reviewprovider.CIObservation{State: tt.state, Reason: tt.reason, Selection: "head", SelectedCommitSHA: "selected-commit", MergeState: "verified", ObservedAt: observed, Assessment: reviewprovider.RequiredCIAssessment{State: tt.state, Reason: tt.reason, Checks: checks}}
			before, err := json.Marshal(o)
			if err != nil {
				t.Fatal(err)
			}
			got := summarizeSourceCI(o)
			want := SourceCISummary{State: tt.state, Reason: tt.wantReason, Selection: o.Selection, SelectedCommitSHA: o.SelectedCommitSHA, MergeState: o.MergeState, ObservedAt: observed.Format(time.RFC3339Nano), Required: int64(3 + len(tt.unproven)), Satisfied: 1, Failed: 1, Pending: 1, Unproven: int64(len(tt.unproven))}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("summary facts changed: got%+v want%+v", got, want)
			}
			after, err := json.Marshal(o)
			if err != nil || string(before) != string(after) {
				t.Fatal("summary changed retained observation", err)
			}
		})
	}
}
