package reviewexec

import (
	"net/http"
	"slices"
	"strconv"
	"testing"

	"github.com/benhook1013/FireController/internal/reviews"
)

func TestQueueCapacityPreservesMergedHistoryAndQueuedPrefix(t *testing.T) {
	f := queueEnrollmentFixture(t)
	queue := make([]int64, 100)
	for i := range queue {
		queue[i] = int64(i + 1)
	}
	configureQueue(t, f, 0, queue...)
	queued := f.enqueue(t, attemptProject, "capacity-existing", "hosted", 2, 1)
	p := projectionPull(1)
	p.State = "merged"
	q := projectionPull(3)
	q.State = "merged"
	for _, pull := range []reviews.PullInput{p, q} {
		key := "capacity-merge-" + strconv.FormatInt(pull.Number, 10)
		rev := configuredQueue(t, f).Revision
		reply := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/queue/edits", f.admin, key, encoded(reviews.QueueEditRequest{ExpectedRevision: &rev, RepositoryID: 801, Source: "fixture", Retired: []reviews.RetiredPull{}, Queue: []reviews.QueuePullInput{{Number: pull.Number, Title: pull.Title, State: pull.State, HeadSHA: pull.HeadSHA, BaseSHA: pull.BaseSHA, BaseRef: pull.BaseRef, CIState: pull.CIState, Mergeability: pull.Mergeability}}}))
		if reply.Code != http.StatusCreated {
			t.Fatal(reply.Code, reply.Body.String())
		}
	}
	got := queueChangeHTTP(t, f, f.writer, "capacity-add", "add", QueueChange{ExpectedConfigurationRevision: 1, PullNumber: 101})
	if got.Revision != 2 {
		t.Fatal(got)
	}
	c, err := f.e.ReadExecutionConfiguration(t.Context(), f.scope(t, attemptProject, f.writer, false))
	if err != nil || len(c.Queue) != 100 || !slices.Equal(c.Queue[:3], []int64{1, 2, 4}) || c.Queue[99] != 101 {
		t.Fatal(c.Queue, err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_current_pulls WHERE number=3`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_current_retired WHERE number=3`) != 0 {
		t.Fatal("merged history was discarded")
	}
	o := bindingOperation(t, f, f.scope(t, attemptProject, f.writer, false), queued.ID)
	if o.State != "queued" || o.ConfigurationRevision != 1 {
		t.Fatal(o)
	}
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	current, revision, err := readConfiguration(t.Context(), tx, attemptProject)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := compatibleQueuedConfiguration(t.Context(), tx, o, current, revision); err != nil || !ok {
		t.Fatal(ok, err)
	}
}
