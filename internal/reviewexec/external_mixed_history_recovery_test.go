package reviewexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

// The source fixture combines original-producer collisions and positive old-head
// discoveries with a partial group, outage, restart and mutable later provider text.
func TestMixedProducerArchiveFailureAndRecovery(t *testing.T) {
	f, schedule, clock := lifecycleFixture(t)
	roundGrant(t, f, 2, "cli", 0, 1, 4, "second-data-cli")
	roundGrant(t, f, 2, "hosted", 0, 1, 3, "second-data-hosted")
	beforeRounds := readProgress(t, f, 2).Items
	var groups []reviews.TransferGroup
	for n, producer := range []string{"original-source-one", "original-source-two"} {
		pull := projectionPull(2)
		pull.Complete = false
		pull.Runs = []reviews.RunInput{{SourceRunID: "shared-original-id", HeadSHA: pull.HeadSHA, Channel: "hosted", Scope: "selected retained source", Outcome: "completed", Attributable: true, Complete: true, Finalized: true, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z", Found: 1, Findings: []reviews.Finding{{SourceFindingID: "shared-finding-id", Title: fmt.Sprintf("Original producer %d", n), Detail: fmt.Sprintf("Exact original detail %d <&>", n), Severity: "low", Disposition: "rejected"}}}}
		groups = append(groups, reviews.TransferGroup{Source: producer, Pulls: []reviews.PullInput{pull}})
	}
	transferProjectionBatch(t, f, "second-data-import", groups)
	var original reviews.RunsResponse
	originalBytes := projectionRead(t, f, "/pulls/2/runs", &original)
	if len(original.Items) != 2 || original.Items[0].ID == original.Items[1].ID || original.Items[0].Source == original.Items[1].Source {
		t.Fatal("canonical transfer lost original producers", original)
	}
	r := externalReader(externalSnapshot(2, strings.Repeat("d", 40)))
	for n := range r.snapshot.Reviews {
		review := &r.snapshot.Reviews[n]
		review.Body = strings.Replace(review.Body, "posted: 0", "posted: 1", 1)
		r.snapshot.ReviewComments = append(r.snapshot.ReviewComments, reviewprovider.ReviewComment{ID: int64(501 + n), ReviewID: review.ID, Author: r.TrustedHostedActor(), HeadSHA: review.HeadSHA, OriginalHeadSHA: review.HeadSHA, Path: "source.go", CreatedAt: review.SubmittedAt, UpdatedAt: review.SubmittedAt, Body: fmt.Sprintf("_⚠️ Potential issue_ | _🟠 Major_\n\n**External source %d.**\n\nExact retained external <&> source.\n\n<!-- cr-comment:v1:%024x -->", n, n+1)})
	}
	// First group is supported; the second group must remain individually held.
	r.snapshot.Reviews[1].Body = strings.Replace(r.snapshot.Reviews[1].Body, "batch=1/1", "batch=1/2", 1)
	if !lifecycleTick(t, f, schedule, r) || externalArchiveCount(t, f) != 1 {
		t.Fatal("partial independent group blocked supported positive source")
	}
	if got := schedule.due[lifecycleKey{attemptProject, 2}].enrichmentError; got != "external_hosted_identity_unproven" {
		t.Fatal("partial group lost diagnostic", got)
	}
	clock.Add(time.Minute)
	r.observe = func(context.Context) (reviewprovider.HostedPublicationSnapshot, error) {
		return reviewprovider.HostedPublicationSnapshot{}, errors.New("controlled provider outage")
	}
	if !lifecycleTick(t, f, schedule, r) || externalArchiveCount(t, f) != 1 {
		t.Fatal("provider outage discarded retained source")
	}
	clock.Add(2 * time.Minute)
	r.observe = nil
	r.snapshot.Reviews[1].Body = strings.Replace(r.snapshot.Reviews[1].Body, "batch=1/2", "batch=1/1", 1)
	schedule = newLifecycleSchedule(schedule.options)
	if !lifecycleTick(t, f, schedule, r) || externalArchiveCount(t, f) != 2 {
		t.Fatal("restart failed to discover completed held group")
	}
	var all reviews.RunsResponse
	projectionRead(t, f, "/pulls/2/runs", &all)
	if len(all.Items) != 4 {
		t.Fatal("mixed retained history lost records", len(all.Items))
	}
	retained := map[string][]byte{}
	for _, run := range all.Items {
		var detail reviews.RunDetailResponse
		retained[run.ID] = projectionRead(t, f, "/runs/"+run.ID+"?snapshot="+all.SnapshotID, &detail)
		if run.Source == "github-hosted-observer" && (run.HeadSHA != strings.Repeat("d", 40) || run.Found != 1 || run.Finalized || len(detail.Findings) != 1 || detail.Findings[0].Disposition != "unadjudicated" || !strings.Contains(detail.Findings[0].Detail, "Exact retained external <&> source.")) {
			t.Fatal("external archive changed original source or invented adjudication", detail)
		}
	}
	// GitHub may edit retained source later. Stable source IDs must retain captured
	// text, and historical snapshot reads must remain byte-for-byte reproducible.
	for n := range r.snapshot.ReviewComments {
		r.snapshot.ReviewComments[n].Body = strings.Replace(r.snapshot.ReviewComments[n].Body, "Exact retained external <&> source.", "Later changed provider text.", 1)
	}
	clock.Add(time.Minute)
	lifecycleTick(t, f, schedule, r)
	for id, expected := range retained {
		var detail reviews.RunDetailResponse
		if got := projectionRead(t, f, "/runs/"+id+"?snapshot="+all.SnapshotID, &detail); !bytes.Equal(got, expected) {
			t.Fatal("historical source changed", id)
		}
		projectionRead(t, f, "/runs/"+id, &detail)
		if detail.Run.Source == "github-hosted-observer" && strings.Contains(detail.Findings[0].Detail, "Later changed") {
			t.Fatal("repeat overwrote captured provider text")
		}
	}
	var originalAgain reviews.RunsResponse
	if got := projectionRead(t, f, "/pulls/2/runs?snapshot="+original.SnapshotID, &originalAgain); !bytes.Equal(got, originalBytes) {
		t.Fatal("original producer snapshot changed")
	}
	if !reflect.DeepEqual(beforeRounds, readProgress(t, f, 2).Items) {
		t.Fatal("external or imported archive consumed independent review allowance")
	}
	for _, table := range []string{"review_operations", "review_execution_attempts", "review_execution_evidence", "review_execution_reservations", "review_round_completions"} {
		if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != 0 {
			t.Fatal("archive manufactured native execution authority", table)
		}
	}
}
