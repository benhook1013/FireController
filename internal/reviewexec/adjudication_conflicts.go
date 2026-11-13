package reviewexec

import "github.com/benhook1013/FireController/internal/core"

func adjudicationRevisionConflict(revision int64) error {
	return &core.Error{Code: "review_conflict", Status: 409, Message: "The adjudication revision differs from expected_revision.", Details: map[string]any{"reason": "adjudication_revision_conflict", "field": "expected_revision", "current_revision": revision}}
}

func findingFixGuardConflict(revision int64, f FindingDecision, reason string) error {
	field, message := "expected_sha", "The finding's currently recorded fix SHA differs from expected_sha."
	if reason == "finding_not_accepted" {
		field, message = "disposition", "Fix proof requires an accepted finding."
	}
	return &core.Error{Code: "review_conflict", Status: 409, Message: message, Details: map[string]any{"reason": reason, "field": field, "current_revision": revision, "finding": f.Finding, "current_fix_sha": f.FixSHA, "current_disposition": f.Disposition}}
}
