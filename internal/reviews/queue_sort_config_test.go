package reviews

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"
)

func TestManualQueueKeepsSavedOrderWithParentLabels(t *testing.T) {
	nodes := []queueNode{componentNode(2, "child", "root"), componentNode(1, "root", "develop"), componentNode(3, "independent", "develop")}
	got, parents := orderedRecentNodesMode(nodes, 701, time.Now(), "manual")
	if !slices.Equal(componentNumbers(got), []int64{2, 1, 3}) || parents[2] != 1 {
		t.Fatal(got, parents)
	}
	auto, _ := orderedRecentNodesMode(nodes, 701, time.Now(), "auto")
	if !slices.Equal(componentNumbers(auto), []int64{3, 1, 2}) {
		t.Fatal(auto)
	}
}
func TestQueueSortingRevisionInvalidatesPagination(t *testing.T) {
	f := newAPIFixture(t)
	f.observe(t, reviewRequest(0, []PullInput{samplePull(1, "one"), samplePull(2, "two"), samplePull(3, "three")}), "sorting-pages")
	_, err := f.store.DB().Exec(`CREATE TABLE review_execution_config(project_id TEXT,revision INTEGER,body_json TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.store.DB().Exec(`INSERT INTO review_execution_config VALUES(?,1,?)`, f.project, `{"queue":[3,1,2],"queue_sort_mode":"manual"}`)
	if err != nil {
		t.Fatal(err)
	}
	get := func(cursor string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews/queue?view=recent&limit=1"+cursor, nil)
		r.Header.Set("Authorization", "Bearer "+f.readToken)
		r.Header.Set("X-FC-Queue-Topology", "v1")
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		return w
	}
	first := get("")
	var page QueueResponse
	if first.Code != 200 || json.Unmarshal(first.Body.Bytes(), &page) != nil || !page.HasMore {
		t.Fatal(first.Code, first.Body.String())
	}
	_, err = f.store.DB().Exec(`INSERT INTO review_execution_config VALUES(?,2,?)`, f.project, `{"queue":[3,1,2],"queue_sort_mode":"auto"}`)
	if err != nil {
		t.Fatal(err)
	}
	stale := get("&cursor=" + url.QueryEscape(page.Cursor))
	if stale.Code == 200 {
		t.Fatal("old page admitted after mode change")
	}
	fresh := get("")
	if fresh.Code != 200 {
		t.Fatal(fresh.Code, fresh.Body.String())
	}
}
