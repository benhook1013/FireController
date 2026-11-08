package reviewexec

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	holdmodel "github.com/benhook1013/FireController/internal/adoptionholdsmodel"
)

func TestPolicyAssessmentCarriedObligationsWithoutPolicy(t *testing.T) {
	for _, configured := range []bool{false, true} {
		for _, obligation := range []string{"absent", "open", "resolved", "unrelated"} {
			t.Run(fmt.Sprintf("configured=%t/%s", configured, obligation), func(t *testing.T) {
				f, a, id, _ := coverageFixture(t, 0, false)
				coverageFinalize(t, f, a, id, 0)
				var revision int64
				state := "unconfigured"
				var channels []PolicyAssessmentChannel
				if configured {
					assessmentPolicy(t, f, 1, []string{"cli"}, []string{})
					revision, state = 2, "sufficient"
					channels = []PolicyAssessmentChannel{{Channel: "cli", Covered: true}}
				} else {
					admissionSQL(t, f, `DELETE FROM review_policy_config WHERE project_id=?`, attemptProject)
				}
				var hold holdmodel.ListItem
				if obligation != "absent" {
					pull := a.Anchor.PullNumber
					if obligation == "unrelated" {
						pull = 4
					}
					_, _, hold = integrationImportHold(t, f, pull)
					if obligation == "resolved" {
						resolution := holdmodel.ResolveRequest{ExpectedRevision: &hold.Revision, Reason: "Correction verified", Evidence: []string{"proof:verified-correction"}}
						w := integrationHoldRequest(t, f, "POST", integrationHoldsPath+"/"+hold.ID+"/resolve", f.admin, "resolve-assessment-hold", resolution)
						if w.Code != http.StatusOK {
							t.Fatalf("resolve assessment hold: %d %s", w.Code, w.Body.String())
						}
					}
				}
				var pending int64
				reason := ""
				if obligation == "open" {
					pending, reason = 1, "source_obligations_pending"
					if configured {
						state = "held"
					}
					for _, channel := range []string{"cli", "hosted"} {
						_, err := f.e.Enqueue(t.Context(), f.scope(t, attemptProject, f.writer, false), "assessment-held-"+channel, Request{ExpectedConfigurationRevision: 1, PullNumber: a.Anchor.PullNumber, Channel: channel, HeadSHA: a.Anchor.CandidateSHA})
						integrationSourceHeld(t, err)
					}
				}
				var before holdmodel.Detail
				if hold.ID != "" {
					before = integrationHoldDetail(t, f, hold.ID)
				}
				got := assessmentRead(t, f, a, id, state, revision, channels...)
				if got.SourceObligationsPending != pending || got.Reason != reason || got.IncomingRoutesPending != 0 || got.IncomingRoutesCapacity {
					t.Fatalf("assessment lost independent carried obligation: %+v", got)
				}
				if hold.ID != "" && !reflect.DeepEqual(before, integrationHoldDetail(t, f, hold.ID)) {
					t.Fatal("assessment rewrote carried obligation provenance or resolution")
				}
				if !configured && attemptCount(t, f.store, `SELECT count(*) FROM review_policy_config WHERE project_id=?`, attemptProject) != 0 {
					t.Fatal("assessment installed an implicit policy")
				}
			})
		}
	}
}

func TestPolicyAssessmentUnconfiguredObligationsPreserveReadGuards(t *testing.T) {
	for _, guard := range []string{"missing-evidence", "foreign-project", "revoked-reader", "malformed-obligation"} {
		t.Run(guard, func(t *testing.T) {
			f, a, id, _ := coverageFixture(t, 0, false)
			coverageFinalize(t, f, a, id, 0)
			admissionSQL(t, f, `DELETE FROM review_policy_config WHERE project_id=?`, attemptProject)
			_, _, hold := integrationImportHold(t, f, a.Anchor.PullNumber)
			scope := evidenceReadScope(t, f, attemptProject, f.reader)
			code := "not_found"
			switch guard {
			case "missing-evidence":
				id = "review_evidence_" + strings.Repeat("0", 32)
			case "foreign-project":
				scope = evidenceReadScope(t, f, otherAttemptProject, f.other)
			case "revoked-reader":
				admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-reader-cred'`)
				code = "unauthorized"
			case "malformed-obligation":
				admissionSQL(t, f, `UPDATE review_adoption_obligations SET revision=2 WHERE id=?`, hold.ID)
				code = "storage_unavailable"
			}
			before := adjudicationState(t, f, a) + policyState(t, f)
			got, err := f.e.ReadPolicyAssessment(t.Context(), scope, id)
			attemptCode(t, err, code)
			if !reflect.DeepEqual(got, PolicyAssessment{}) || before != adjudicationState(t, f, a)+policyState(t, f) {
				t.Fatal("failed assessment exposed facts or changed durable state", got)
			}
		})
	}
}
