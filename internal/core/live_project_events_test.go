package core

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func TestLiveProjectReplayVisibleStateOnly(t *testing.T) {
	s, scope, _ := streamFixture(t)
	// Keep this explicit wire contract independent of the replay query: an
	// arbitrary journal event must never become a private browser notification.
	kinds := []string{
		"project.presentation_changed", "map.entry_created", "map.entry_updated",
		"job.workstream_changed", "job.created", "job.updated", "job.checkpointed", "job.update_appended", "job.checklist_replaced",
		"workers.created", "worker.pause_changed", "worker.primary_job_changed", "note.created", "note.updated",
		"review.observed", "reviews.policy_configured", "reviews.rounds_configured",
		"review.evidence.adjudicated", "review.evidence.finalized", "review.evidence.fix_recorded", "review.evidence.routed",
		"review.route.routed", "review.route.superseded", "review.route.adjudicated", "review.route.fix_recorded",
	}
	for i, kind := range kinds {
		commitProjectEvent(t, s, scope, kind, fmt.Sprintf("visible-%d", i))
	}
	// All of these are real journal namespaces but none invalidates a current
	// browser view. A future same-prefix event is not implicitly opted in.
	for i, kind := range []string{
		"message.sent", "message.seen", "message.acknowledged", "credentials.revoked",
		"reviews.configured", "review.requested", "review.captured", "review.capture_observed",
		"review.source_refresh_requested", "review.source_refresh_updated", "reviews.checkpoint_publication",
		"review.cli_preflight", "review.hosted_preflight", "review.hosted_query_started", "review.hosted_query_posted", "review.hosted_settled",
		"job.future", "review.evidence.future", "review.route.future",
	} {
		commitProjectEvent(t, s, scope, kind, fmt.Sprintf("excluded-%d", i))
	}
	if _, err := s.DB().Exec(`INSERT INTO projects VALUES('foreign','Foreign',2,'synthetic/foreign'); INSERT INTO workers VALUES('foreign','worker','Worker'); INSERT INTO grants VALUES('credential','foreign',1,1)`); err != nil {
		t.Fatal(err)
	}
	foreign := scope
	foreign.ProjectID = "foreign"
	commitProjectEvent(t, s, foreign, "job.updated", "foreign-private")
	var got []string
	var after int64
	for {
		page, err := s.readProjectEvents(context.Background(), scope, after, 7)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) > 7 {
			t.Fatalf("unbounded page: %d", len(page.Items))
		}
		for _, item := range page.Items {
			got = append(got, item.EventType)
			body, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(body, &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 4 || fields["event_type"] == nil || fields["resource_id"] == nil || fields["revision"] == nil || fields["event_cursor"] == nil {
				t.Fatalf("event exposed more than compact invalidation envelope: %s", body)
			}
		}
		position, err := s.eventPosition(scope, projectFeed, page.EventCursor)
		if err != nil || position <= after {
			t.Fatalf("cursor did not advance: %d -> %d: %v", after, position, err)
		}
		after = position
		if !page.HasMore {
			if page.EventCursor != projectCursor(t, s, scope) {
				t.Fatal("filtered tail did not advance to scoped snapshot")
			}
			break
		}
	}
	if !reflect.DeepEqual(got, kinds) {
		t.Fatalf("visible replay changed or private event escaped:\ngot %v\nwant %v", got, kinds)
	}
}

func TestLiveProjectHintsUseSameScopedAllowlist(t *testing.T) {
	s, scope, _ := streamFixture(t)
	project, releaseProject, err := s.addEventWaiter(scope, messageTopic{project: scope.ProjectID, feed: projectFeed})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseProject()
	foreign, releaseForeign, err := s.addEventWaiter(scope, messageTopic{project: "foreign", feed: projectFeed})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseForeign()
	message, releaseMessage, err := s.addMessageWaiter(scope)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseMessage()
	for i, kind := range projectEventTypes {
		commitProjectEvent(t, s, scope, kind, fmt.Sprintf("hint-%d", i))
		if len(project.ready) != 1 || len(foreign.ready) != 0 || len(message.ready) != 0 {
			t.Fatalf("wrong notification namespace for %s", kind)
		}
		<-project.ready
	}
	commitProjectEvent(t, s, scope, "review.source_refresh_updated", "no-visible-change")
	if len(project.ready) != 0 || len(foreign.ready) != 0 || len(message.ready) != 0 {
		t.Fatal("internal provider state woke browser or messages")
	}
}
