package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestActiveQueueFiltersBeforePagingPinsSnapshotAndConserves(t *testing.T) {
	f := newAPIFixture(t)
	base := importBase(f)
	if _, err := f.store.DB().Exec(`INSERT INTO projects(id,name,github_repository_id,repository) VALUES('active-other-cursor','Other',702,'owner/other')`); err != nil {
		t.Fatal(err)
	}
	empty := f.call("GET", base+"/queue/active", f.readToken, "", nil)
	var initial ActiveQueueResponse
	if empty.Code != 200 || json.Unmarshal(empty.Body.Bytes(), &initial) != nil || initial.Configured || initial.Items == nil || len(initial.Items) != 0 || initial.SnapshotID != "" || initial.Revision != 0 || initial.HasMore {
		t.Fatal("invalid unconfigured queue", empty.Body.String())
	}
	var pulls []PullInput
	for n := int64(1); n <= 20; n++ {
		p := samplePull(n, fmt.Sprintf("Pull %d", n))
		if n > 10 {
			p.State = "open"
		}
		if n == 12 {
			p.State = "closed"
		}
		if n == 13 {
			p.State = "unknown"
		}
		if n == 11 {
			p.Runs = []RunInput{{SourceRunID: "private-run", Channel: "manual", Scope: "private-scope", Outcome: "unknown", Found: 1, Findings: []Finding{{SourceFindingID: "private-finding", Title: "Private title", Detail: "PRIVATE_FINDING_DETAIL", Severity: "unknown", Disposition: "unadjudicated"}}}}
		}
		pulls = append(pulls, p)
	}
	f.observe(t, reviewRequest(0, pulls), "active-initial")
	if r := queueEditHTTP(f, queueEditRequest(1, nil, []RetiredPull{{Number: 15, Reason: "defunct"}}), "active-retire"); r.Code != 201 {
		t.Fatal(r.Code, r.Body.String())
	}
	// Native retention may leave an open parent record beside its explicit
	// retirement. That record must remain invisible in both queue views.
	scope := transferTestScope(t, f)
	native := NativePublication{Pull: PullInput{Number: 15, Title: "Retired open pull", State: "open", CIState: "unknown", Mergeability: "unknown"}, Attempt: &Attempt{SourceAttemptID: "private-retired-attempt", Channel: "cli", State: "unknown"}}
	if _, err := f.store.Mutate(t.Context(), scope, core.Mutation{Operation: "test.active.retained", Target: "workspace", Key: "active-retained", Request: native}, func(ctx context.Context, tx *sql.Tx) (any, error) { return PublishNative(ctx, tx, scope, native) }); err != nil {
		t.Fatal(err)
	}
	read := func(path string) ActiveQueueResponse {
		t.Helper()
		r := f.call("GET", path, f.readToken, "", nil)
		var out ActiveQueueResponse
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &out) != nil {
			t.Fatal(r.Code, r.Body.String())
		}
		for _, hidden := range []string{"PRIVATE_FINDING_DETAIL", "private-scope", `"recent_runs"`, `"observed_at"`, `"state"`, `"source"`, `"complete"`, `"observer"`} {
			if strings.Contains(r.Body.String(), hidden) {
				t.Fatal("unselected data in active queue", hidden)
			}
		}
		return out
	}
	before := importDatabaseState(t, f)
	first := read(base + "/queue/active")
	if !first.Configured || len(first.Items) != 5 || !first.HasMore || first.Cursor == "" || first.Items[0].Number != 11 || first.Items[0].Position != 10 || first.Items[0].RunCount != 1 {
		t.Fatal("merged prefix consumed active page", first)
	}
	if r := f.call("GET", "/api/v1/projects/active-other-cursor/reviews/queue/active?cursor="+url.QueryEscape(first.Cursor), f.admin, "", nil); r.Code != 400 {
		t.Fatal("signed cursor crossed project scope", r.Code)
	}
	if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatal("active read wrote state")
	}
	// Publish new current display facts; the old continuation must retain its
	// snapshot and every original open entry exactly once.
	change := queueEditRequest(first.Revision, []QueuePullInput{{Number: 20, Title: "Now merged", State: "merged", CIState: "unknown", Mergeability: "unknown"}, {Number: 21, Title: "New current open", State: "open", CIState: "unknown", Mergeability: "unknown"}}, nil)
	if r := queueEditHTTP(f, change, "active-new-current"); r.Code != 201 {
		t.Fatal(r.Code, r.Body.String())
	}
	before = importDatabaseState(t, f)
	seen := []int64{}
	for _, p := range first.Items {
		seen = append(seen, p.Number)
	}
	tail := read(base + "/queue/active?limit=100&cursor=" + url.QueryEscape(first.Cursor))
	if tail.SnapshotID != first.SnapshotID || tail.Revision != first.Revision || tail.HasMore || tail.Cursor != "" {
		t.Fatal("continuation escaped pinned snapshot", tail)
	}
	for _, p := range tail.Items {
		seen = append(seen, p.Number)
	}
	if !reflect.DeepEqual(seen, []int64{11, 14, 16, 17, 18, 19, 20}) {
		t.Fatal("missing/duplicate/inactive entries", seen)
	}
	current := read(base + "/queue/active?limit=100")
	if current.SnapshotID == first.SnapshotID || current.Items[len(current.Items)-1].Number != 21 {
		t.Fatal("current view failed to advance", current)
	}
	full := f.call("GET", base+"/queue?limit=1", f.readToken, "", nil)
	var legacy QueueResponse
	if full.Code != 200 || json.Unmarshal(full.Body.Bytes(), &legacy) != nil || legacy.Items[0].State != "merged" {
		t.Fatal("full queue changed")
	}
	for _, path := range []string{base + "/queue/active?cursor=" + url.QueryEscape(legacy.Cursor), base + "/queue?cursor=" + url.QueryEscape(first.Cursor), base + "/queue/active?cursor=forged", base + "/queue/active?limit=0", base + "/queue/active?limit=101", base + "/queue/active?limit=1&limit=2", base + "/queue/active?cursor=x&cursor=y", base + "/queue/active?view=recent"} {
		if r := f.call("GET", path, f.readToken, "", nil); r.Code != 400 {
			t.Fatal("accepted wrong/malformed selection", path, r.Code)
		}
	}
	if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatal("queue reads changed retained state")
	}
}

func TestActiveQueueScopeAndConfiguredEmpty(t *testing.T) {
	f := newAPIFixture(t)
	base := importBase(f)
	if r := f.call("GET", base+"/queue/active", "", "", nil); r.Code != 401 {
		t.Fatal(r.Code)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO projects(id,name,github_repository_id,repository) VALUES('active-other','Other',702,'owner/other')`); err != nil {
		t.Fatal(err)
	}
	f.observe(t, reviewRequest(0, []PullInput{samplePull(1, "Merged history")}), "active-no-open")
	if r := f.call("GET", "/api/v1/projects/active-other/reviews/queue/active", f.readToken, "", nil); r.Code != 403 {
		t.Fatal("cross-project read", r.Code)
	}
	r := f.call("GET", base+"/queue/active", f.readToken, "", nil)
	var out ActiveQueueResponse
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &out) != nil || !out.Configured || out.Revision != 1 || out.SnapshotID == "" || out.Items == nil || len(out.Items) != 0 || out.HasMore {
		t.Fatal("configured empty active queue", r.Code, r.Body.String())
	}
}
