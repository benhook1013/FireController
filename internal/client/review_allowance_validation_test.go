package client

import (
	"strings"
	"testing"
)

func TestRetainedAllowanceValidation(t *testing.T) {
	zero, limit, full, window := int64(0), int64(8), int64(1000), int64(3600000)
	baseline := ReviewOperation{Channel: "cli", State: "held", Reason: "allowance_exhausted", AttemptID: "review_attempt_" + strings.Repeat("a", 32), Allowance: &ReviewAllowanceObservation{Kind: "cli_exhausted", ObservedAt: "2026-01-01T00:00:00Z", Remaining: &zero, Limit: &limit, FullCapacityInMS: &full, RollingWindowMS: &window}}
	for _, test := range []struct {
		name   string
		mutate func(*ReviewOperation)
		valid  bool
	}{
		{"valid", func(*ReviewOperation) {}, true},
		{"absent legacy observation", func(v *ReviewOperation) { v.Allowance = nil }, true},
		{"queued cannot be terminal exhaustion", func(v *ReviewOperation) { v.State = "queued" }, false},
		{"wrong channel", func(v *ReviewOperation) { v.Channel = "hosted" }, false},
		{"missing attempt", func(v *ReviewOperation) { v.AttemptID = "" }, false},
		{"missing observed time", func(v *ReviewOperation) { v.Allowance.ObservedAt = "" }, false},
		{"noncanonical time", func(v *ReviewOperation) { v.Allowance.ObservedAt = "2026-01-01T00:00:00+00:00" }, false},
		{"missing full capacity", func(v *ReviewOperation) { v.Allowance.FullCapacityInMS = nil }, false},
		{"cli cannot claim reset", func(v *ReviewOperation) { v.Allowance.RetryNotBefore = "2026-01-01T01:00:00Z" }, false},
		{"nonzero remaining", func(v *ReviewOperation) { n := int64(1); v.Allowance.Remaining = &n }, false},
		{"zero limit", func(v *ReviewOperation) { n := int64(0); v.Allowance.Limit = &n }, false},
		{"limit above maximum", func(v *ReviewOperation) { n := int64(10001); v.Allowance.Limit = &n }, false},
		{"negative full capacity", func(v *ReviewOperation) { n := int64(-1); v.Allowance.FullCapacityInMS = &n }, false},
		{"full capacity above maximum", func(v *ReviewOperation) { n := int64(86400001); v.Allowance.FullCapacityInMS = &n }, false},
		{"zero rolling window", func(v *ReviewOperation) { n := int64(0); v.Allowance.RollingWindowMS = &n }, false},
		{"rolling window above maximum", func(v *ReviewOperation) { n := int64(86400001); v.Allowance.RollingWindowMS = &n }, false},
		{"minimum limit", func(v *ReviewOperation) { n := int64(1); v.Allowance.Limit = &n }, true},
		{"maximum limit", func(v *ReviewOperation) { n := int64(10000); v.Allowance.Limit = &n }, true},
		{"minimum full capacity", func(v *ReviewOperation) { n := int64(0); v.Allowance.FullCapacityInMS = &n }, true},
		{"maximum full capacity", func(v *ReviewOperation) { n := int64(86400000); v.Allowance.FullCapacityInMS = &n }, true},
		{"minimum rolling window", func(v *ReviewOperation) { n := int64(1); v.Allowance.RollingWindowMS = &n }, true},
		{"maximum rolling window", func(v *ReviewOperation) { n := int64(86400000); v.Allowance.RollingWindowMS = &n }, true},
		{"missing rolling window", func(v *ReviewOperation) { v.Allowance.RollingWindowMS = nil }, false},
		{"unknown kind", func(v *ReviewOperation) { v.Allowance.Kind = "available" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			v := baseline
			a := *baseline.Allowance
			v.Allowance = &a
			test.mutate(&v)
			if validReviewAllowance(v) != test.valid {
				t.Fatal("incorrect allowance acceptance")
			}
		})
	}
	hosted := ReviewOperation{Channel: "hosted", State: "held", Reason: "hosted_allowance_cooldown", AttemptID: baseline.AttemptID, Allowance: &ReviewAllowanceObservation{Kind: "hosted_cooldown", ObservedAt: "2026-01-01T00:00:00Z", RetryNotBefore: "2026-01-01T01:00:00Z"}}
	if !validReviewAllowance(hosted) {
		t.Fatal("valid hosted cooldown rejected")
	}
	for _, retry := range []string{"", "2026-01-01T01:00:00+00:00"} {
		candidate := hosted
		a := *hosted.Allowance
		candidate.Allowance = &a
		candidate.Allowance.RetryNotBefore = retry
		if validReviewAllowance(candidate) {
			t.Fatal("hosted cooldown accepted missing/noncanonical retry", retry)
		}
	}
	hosted.Allowance.Remaining = &zero
	if validReviewAllowance(hosted) {
		t.Fatal("hosted cooldown accepted invented CLI balance")
	}
}
