package reviewexec

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestOperationAttentionRetainsCompletedWorkThroughFinalizationAndFix(t *testing.T) {
	f, a, evidence := adjudicationFixture(t, 1, false)
	var err error
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "capture_settled", "")
	if err != nil {
		t.Fatal(err)
	}
	// Settled history newer than this result must be filtered before LIMIT, not
	// read as a latest-five sample that silently drops the older obligation.
	for n := 1; n <= 9; n++ {
		id := seedOperationListRow(t, f, n, attemptProject, "completed", "cli", 2)
		admissionSQL(t, f, `UPDATE review_operations SET created_at='2099-01-01T00:00:00Z',updated_at='2099-01-01T00:00:00Z' WHERE id=?`, id)
	}
	pending := seedOperationListRow(t, f, 20, attemptProject, "queued", "cli", 2)
	admissionSQL(t, f, `UPDATE review_operations SET created_at='2099-01-02T00:00:00Z',updated_at='2099-01-02T00:00:00Z' WHERE id=?`, pending)
	other := seedOperationListRow(t, f, 21, attemptProject, "queued", "hosted", 3)
	admissionSQL(t, f, `UPDATE review_operations SET created_at='2099-01-03T00:00:00Z',updated_at='2099-01-03T00:00:00Z' WHERE id=?`, other)
	before := evidenceState(t, f, a)
	all := listOperationPage(t, f, "?status=all")
	for _, item := range all.Items {
		if item.ID == a.OperationID {
			t.Fatal("fixture did not hide old result behind latest-five history")
		}
	}
	page := listOperationPage(t, f, "?status=attention&channel=cli&pull_number=2&limit=1")
	if len(page.Items) != 1 || page.Items[0].ID != pending || !page.HasMore || page.Cursor == "" {
		t.Fatal(page)
	}
	cursor := "&cursor=" + url.QueryEscape(page.Cursor)
	next := listOperationPage(t, f, "?status=attention&channel=cli&pull_number=2&limit=1"+cursor)
	if len(next.Items) != 1 || next.Items[0].ID != a.OperationID || next.Items[0].State != "completed" || next.HasMore {
		t.Fatal("old completed review disappeared before adjudication", next)
	}
	// Existing raw HTTP and explicit pending callers retain provider-only rows.
	for _, query := range []string{"", "?status=pending"} {
		provider := listOperationPage(t, f, query)
		if len(provider.Items) != 2 {
			t.Fatal("pending selection changed", provider)
		}
		for _, item := range provider.Items {
			if item.State == "completed" {
				t.Fatal("old pending response contains completed state")
			}
		}
	}
	if before != evidenceState(t, f, a) {
		t.Fatal("discovery changed evidence or accounting")
	}
	base := "/api/v1/projects/" + attemptProject + "/reviews/operations"
	for _, query := range []string{"?status=pending&channel=cli&pull_number=2", "?status=all&channel=cli&pull_number=2", "?status=attention&channel=hosted&pull_number=2", "?status=attention&channel=cli&pull_number=3"} {
		if response := f.request("GET", base+query+cursor, f.reader, "", ""); response.Code != 400 {
			t.Fatal("cursor lost filter binding", query, response.Code)
		}
	}
	scope := f.scope(t, attemptProject, f.writer, false)
	if _, err = f.e.DecideReviewFinding(t.Context(), scope, "attention-decide", evidence, FindingDecisionRequest{ExpectedRevision: adjudicationRevision(0), Finding: 0, Disposition: "accepted", Reason: "Verified actual defect"}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.FinalizeReviewEvidence(t.Context(), scope, "attention-finalize", evidence, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)}); err != nil {
		t.Fatal(err)
	}
	next = listOperationPage(t, f, "?status=attention&channel=cli&pull_number=2&limit=1"+cursor)
	if len(next.Items) != 1 || next.Items[0].ID != a.OperationID {
		t.Fatal("finalization hid accepted unfinished fix", next)
	}
	if _, err = f.e.RecordFindingFix(t.Context(), scope, "attention-fix", evidence, FindingFixRequest{ExpectedRevision: adjudicationRevision(2), Finding: 0, ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("f", 40), ProofNote: "Focused proof passed"}); err != nil {
		t.Fatal(err)
	}
	if next = listOperationPage(t, f, "?status=attention&channel=cli&pull_number=2&limit=1"+cursor); len(next.Items) != 0 || next.HasMore {
		t.Fatal("settled fixed review stayed in attention", next)
	}
}

func TestOperationAttentionCleanResultsAndNativeSourceIsolation(t *testing.T) {
	for _, count := range []int{0, 1} {
		t.Run(fmt.Sprintf("findings=%d", count), func(t *testing.T) {
			f, a, evidence := adjudicationFixture(t, count, false)
			var err error
			a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "capture_settled", "")
			if err != nil {
				t.Fatal(err)
			}
			scope := f.scope(t, attemptProject, f.writer, false)
			revision := int64(0)
			if count != 0 {
				if _, err = f.e.DecideReviewFinding(t.Context(), scope, "attention-reject", evidence, FindingDecisionRequest{ExpectedRevision: &revision, Finding: 0, Disposition: "rejected", Reason: "Verified false positive"}); err != nil {
					t.Fatal(err)
				}
				revision++
			}
			if page := listOperationPage(t, f, "?status=attention"); len(page.Items) != 1 || page.Items[0].ID != a.OperationID {
				t.Fatal("unfinalized clean result hidden", page)
			}
			// A foreign project or wrong original attempt cannot supply attention credit
			// to this operation. This corruption exists only in the disposable fixture.
			admissionSQL(t, f, `UPDATE review_execution_evidence SET project_id=? WHERE id=?`, otherAttemptProject, evidence)
			if page := listOperationPage(t, f, "?status=attention"); len(page.Items) != 0 {
				t.Fatal("foreign evidence supplied operation attention", page)
			}
			admissionSQL(t, f, `UPDATE review_execution_evidence SET project_id=? WHERE id=?`, attemptProject, evidence)
			if _, err = f.e.FinalizeReviewEvidence(t.Context(), scope, "attention-finalize-clean", evidence, FinalizeReviewRequest{ExpectedRevision: &revision}); err != nil {
				t.Fatal(err)
			}
			if page := listOperationPage(t, f, "?status=attention"); len(page.Items) != 0 {
				t.Fatal("finalized clean result stayed in attention", page)
			}
		})
	}
}

func TestOperationAttentionMalformedStopAndFixFactsCannotHideWork(t *testing.T) {
	f, a, evidence := adjudicationFixture(t, 1, false)
	var err error
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "capture_settled", "")
	if err != nil {
		t.Fatal(err)
	}
	scope := f.scope(t, attemptProject, f.writer, false)
	if _, err = f.e.DecideReviewFinding(t.Context(), scope, "attention-corrupt-decide", evidence, FindingDecisionRequest{ExpectedRevision: adjudicationRevision(0), Finding: 0, Disposition: "accepted", Reason: "Verified defect"}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.FinalizeReviewEvidence(t.Context(), scope, "attention-corrupt-finalize", evidence, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.RecordFindingFix(t.Context(), scope, "attention-corrupt-fix", evidence, FindingFixRequest{ExpectedRevision: adjudicationRevision(2), Finding: 0, ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("f", 40), ProofNote: "Valid retained fix"}); err != nil {
		t.Fatal(err)
	}
	if page := listOperationPage(t, f, "?status=attention"); len(page.Items) != 0 {
		t.Fatal("valid fixed history should be excluded", page)
	}
	var original string
	if err = f.store.DB().QueryRow(`SELECT body_json FROM review_adjudication_state WHERE evidence_id=?`, evidence).Scan(&original); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/projects/" + attemptProject + "/reviews/operations?status=attention"
	// Both canonical producer forms stay excluded once all work is settled.
	for _, fixedAt := range []string{"2026-11-01T12:00:00Z", "2026-11-01T12:00:00.123456789Z"} {
		admissionSQL(t, f, `UPDATE review_adjudication_state SET body_json=json_set(body_json,'$.findings[0].fixed_at',?) WHERE evidence_id=?`, fixedAt, evidence)
		if page := listOperationPage(t, f, "?status=attention"); len(page.Items) != 0 {
			t.Fatal("valid UTC fix timestamp became attention", fixedAt, page)
		}
	}
	admissionSQL(t, f, `UPDATE review_adjudication_state SET body_json=? WHERE evidence_id=?`, original, evidence)
	for name, update := range map[string]string{
		"empty-object":        `'{}'`,
		"missing-finalized":   `json_remove(body_json,'$.finalized')`,
		"null-finalized":      `json_set(body_json,'$.finalized',NULL)`,
		"string-finalized":    `json_set(body_json,'$.finalized','true')`,
		"numeric-finalized":   `json_set(body_json,'$.finalized',1)`,
		"missing-findings":    `json_remove(body_json,'$.findings')`,
		"null-findings":       `json_set(body_json,'$.findings',NULL)`,
		"short-fix-sha":       `json_set(body_json,'$.findings[0].fix_sha','PRIVATE_BAD_SHA')`,
		"invalid-hex-fix-sha": `json_set(body_json,'$.findings[0].fix_sha','fffffffffffffffffffffffffffffffffffffffg')`,
		"numeric-fix-sha":     `json_set(body_json,'$.findings[0].fix_sha',1)`,
		"missing-fix-sha":     `json_remove(body_json,'$.findings[0].fix_sha')`,
		"missing-proof":       `json_set(body_json,'$.findings[0].proof_note','')`,
		"invalid-fix-time":    `json_set(body_json,'$.findings[0].fixed_at','PRIVATE_BAD_TIME')`,
		"relative-fix-time":   `json_set(body_json,'$.findings[0].fixed_at','now')`,
		"date-only-fix-time":  `json_set(body_json,'$.findings[0].fixed_at','2026-11-01')`,
		"spaced-fix-time":     `json_set(body_json,'$.findings[0].fixed_at','2026-11-01T12:00:00 Z')`,
		"empty-fix-fraction":  `json_set(body_json,'$.findings[0].fixed_at','2026-11-01T12:00:00.Z')`,
	} {
		t.Run(name, func(t *testing.T) {
			admissionSQL(t, f, `UPDATE review_adjudication_state SET body_json=`+update+` WHERE evidence_id=?`, evidence)
			before := evidenceState(t, f, a)
			response := f.request("GET", base, f.reader, "", "")
			if response.Code != 500 || strings.Contains(response.Body.String(), "PRIVATE_") {
				t.Fatal("corruption silently hid unfinished work", response.Code, response.Body.String())
			}
			if before != evidenceState(t, f, a) {
				t.Fatal("failed attention discovery changed records")
			}
			admissionSQL(t, f, `UPDATE review_adjudication_state SET body_json=? WHERE evidence_id=?`, original, evidence)
		})
	}
	// The lookahead row is also validated, rather than advertising a page whose
	// continuation would conceal a corrupt completed operation.
	pending := seedOperationListRow(t, f, 81, attemptProject, "queued", "cli", 2)
	admissionSQL(t, f, `UPDATE review_operations SET created_at='2099-01-01T00:00:00Z',updated_at='2099-01-01T00:00:00Z' WHERE id=?`, pending)
	admissionSQL(t, f, `UPDATE review_adjudication_state SET body_json='{}' WHERE evidence_id=?`, evidence)
	if response := f.request("GET", base+"&limit=1", f.reader, "", ""); response.Code != 500 {
		t.Fatal("corrupt lookahead became an apparently valid page", response.Code, response.Body.String())
	}
}

func TestOperationAttentionCompletedEvidenceRequiresOriginalBinding(t *testing.T) {
	f, a, evidence := adjudicationFixture(t, 1, false)
	var err error
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "capture_settled", "")
	if err != nil {
		t.Fatal(err)
	}
	var original string
	if err = f.store.DB().QueryRow(`SELECT body_json FROM review_execution_evidence WHERE id=?`, evidence).Scan(&original); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, field, value string }{
		{"body-id", "$.id", "review_evidence_" + strings.Repeat("f", 32)},
		{"body-project", "$.project_id", otherAttemptProject},
		{"body-operation", "$.operation_id", "review_operation_" + strings.Repeat("f", 32)},
		{"body-attempt", "$.attempt_id", "review_attempt_" + strings.Repeat("f", 32)},
		{"body-head", "$.anchor.candidate_sha", strings.Repeat("f", 40)},
		{"body-parent", "$.anchor.parent_sha", strings.Repeat("f", 40)},
		{"expected-anchor", "$.capture.expected_anchor.candidate_sha", strings.Repeat("f", 40)},
		{"capability", "$.capability_fingerprint", strings.Repeat("f", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			admissionSQL(t, f, `UPDATE review_execution_evidence SET body_json=json_set(body_json,?,?) WHERE id=?`, tc.field, tc.value, evidence)
			before := evidenceState(t, f, a)
			response := f.request("GET", "/api/v1/projects/"+attemptProject+"/reviews/operations?status=attention", f.reader, "", "")
			if response.Code != 500 {
				t.Fatal("foreign retained evidence appeared as current attention", tc.name, response.Code, response.Body.String())
			}
			if before != evidenceState(t, f, a) {
				t.Fatal("binding refusal changed retained records")
			}
			admissionSQL(t, f, `UPDATE review_execution_evidence SET body_json=? WHERE id=?`, original, evidence)
		})
	}
}
