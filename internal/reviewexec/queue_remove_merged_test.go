package reviewexec

import (
	"bytes"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviews"
)

func TestQueueRemovalExplicitSupersededMergedPreservesEvidence(t *testing.T) {
	f := queueEnrollmentFixture(t)
	carrier, ordinary := projectionPull(2), projectionPull(4)
	carrier.State, ordinary.State = "merged", "merged"
	carrier.Runs = []reviews.RunInput{{SourceRunID: "retained-carrier-review", Channel: "cli", HeadSHA: carrier.HeadSHA, Reviewer: "Independent", Scope: "patch", Outcome: "completed", StartedAt: "2026-01-01T01:00:00Z", FinishedAt: "2026-01-01T01:02:00Z", Attributable: true, Complete: true, Finalized: true, Found: 1, Accepted: 1, Findings: []reviews.Finding{{SourceFindingID: "retained-finding", Title: "Retained evidence", Detail: "Original source detail", Severity: "medium", Disposition: "accepted", ProofNote: "Retained focused proof"}}}}
	observation := reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: 801, Source: "merged-carriers", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{carrier, ordinary}}
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "original-merged", encoded(observation))
	if w.Code != http.StatusCreated {
		t.Fatal(w.Code, w.Body.String())
	}
	configureQueue(t, f, 0, 2, 4)
	before := configuredQueue(t, f)
	var pull reviews.PullResponse
	pinned := projectionRead(t, f, "/pulls/2?snapshot="+before.SnapshotID, &pull)
	var runs reviews.RunsResponse
	projectionRead(t, f, "/pulls/2/runs", &runs)
	if len(runs.Items) != 1 {
		t.Fatal("retained source review missing", runs)
	}
	var run reviews.RunDetailResponse
	runRoute := "/runs/" + runs.Items[0].ID + "?snapshot=" + before.SnapshotID
	evidence := projectionRead(t, f, runRoute, &run)
	control2, control4 := roundRead(t, f, 2, "cli"), roundRead(t, f, 4, "hosted")
	route := "/api/v1/projects/" + attemptProject + "/reviews/execution/queue/remove"
	request := QueueChange{ExpectedConfigurationRevision: 1, PullNumber: 2, Reason: "superseded"}
	original := projectionDigest(t, f, "") + enrollmentState(t, f)
	for _, tc := range []struct {
		token    string
		revision int64
		status   int
	}{{f.reader, 1, 403}, {f.other, 1, 403}, {f.writer, 2, 409}} {
		refused := request
		refused.ExpectedConfigurationRevision = tc.revision
		w := f.request("POST", route, tc.token, "merged-refusal", encoded(refused))
		if w.Code != tc.status || original != projectionDigest(t, f, "")+enrollmentState(t, f) {
			t.Fatal("merged retirement weakened authorization/CAS", w.Code, w.Body.String())
		}
	}
	admissionSQL(t, f, "CREATE TRIGGER merged_retirement_failure BEFORE INSERT ON review_retired_versions BEGIN SELECT RAISE(ABORT,'retirement rollback'); END")
	w = f.request("POST", route, f.writer, "superseded-carrier", encoded(request))
	if w.Code < 400 || original != projectionDigest(t, f, "")+enrollmentState(t, f) {
		t.Fatal("merged retirement partially committed", w.Code, w.Body.String())
	}
	admissionSQL(t, f, "DROP TRIGGER merged_retirement_failure")
	first := queueChangeHTTP(t, f, f.writer, "superseded-carrier", "remove", request)
	if first.Revision != 2 || !reflect.DeepEqual(queueNumbers(configuredQueue(t, f)), []int64{4}) {
		t.Fatal("ordinary merged history was removed", first)
	}
	if !bytes.Equal(pinned, projectionRead(t, f, "/pulls/2?snapshot="+before.SnapshotID, &pull)) || !bytes.Equal(evidence, projectionRead(t, f, runRoute, &run)) || !reflect.DeepEqual(roundRead(t, f, 2, "cli"), control2) || !reflect.DeepEqual(roundRead(t, f, 4, "hosted"), control4) {
		t.Fatal("retirement changed historical evidence or controls")
	}
	after := projectionDigest(t, f, "") + enrollmentState(t, f)
	if replay := queueChangeHTTP(t, f, f.writer, "superseded-carrier", "remove", request); replay != first || after != projectionDigest(t, f, "")+enrollmentState(t, f) {
		t.Fatal("retirement replay duplicated publication", replay)
	}
	var reason, actor string
	var retirements, events int
	if err := f.store.DB().QueryRow("SELECT count(*),min(reason) FROM review_retired_versions WHERE project_id=? AND number=2", attemptProject).Scan(&retirements, &reason); err != nil || retirements != 1 || reason != "superseded" {
		t.Fatal("retirement reason lost", retirements, reason, err)
	}
	if err := f.store.DB().QueryRow("SELECT count(*),min(actor_id) FROM events WHERE project_id=? AND event_type='review.observed' AND json_extract(payload_json,'$.source')='execution-queue-removal'", attemptProject).Scan(&events, &actor); err != nil || events != 1 || actor != f.scope(t, attemptProject, f.writer, false).PrincipalID {
		t.Fatal("retirement attribution/replay lost", events, actor, err)
	}
	if err := reviews.ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
}
