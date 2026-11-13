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

func TestQueueTopologyBeforePaging(t *testing.T) {
	f := newAPIFixture(t)
	nodes := []struct {
		number     int64
		head, base string
		draft      bool
	}{
		{3427, "root", "develop", false}, {3408, "first", "root", false},
		{3424, "second", "root", false}, {3410, "receiving", "first", false},
		{3438, "donor", "unenrolled", true}, {3417, "standalone", "develop", false},
		{3435, "future", "first", true}, {3425, "second-child", "second", false},
	}
	pulls := []PullInput{}
	for _, n := range nodes {
		p := samplePull(n.number, n.head)
		p.State, p.BaseRef = "open", n.base
		pulls = append(pulls, p)
	}
	for i, n := range []int64{3430, 3433} {
		p := samplePull(n, "merged")
		p.MergedAt = time.Now().UTC().Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		pulls = append(pulls, p)
	}
	f.observe(t, reviewRequest(0, pulls), "topology-fixture")
	for _, n := range nodes {
		body, err := json.Marshal(PullDisplayMetadata{IsDraft: &n.draft, Branches: &PullBranches{HeadRepositoryID: 701, HeadRef: n.head, DeliveryRef: "develop"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.store.DB().Exec(`UPDATE review_pull_versions SET metadata_json=? WHERE project_id=? AND number=?`, string(body), f.project, n.number); err != nil {
			t.Fatal(err)
		}
	}
	get := func(cursor string) QueueResponse {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews/queue?view=recent&limit=3"+cursor, nil)
		r.Header.Set("Authorization", "Bearer "+f.readToken)
		r.Header.Set("X-FC-Queue-Topology", "v1")
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		var page QueueResponse
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatalf("queue: %d %s", w.Code, w.Body.String())
		}
		return page
	}
	var all []int64
	parents := map[int64]int64{}
	for cursor := ""; ; {
		page := get(cursor)
		if page.HeadPosition == nil || *page.HeadPosition != 2 {
			t.Fatal("wrong display head", page.HeadPosition)
		}
		for _, p := range page.Items {
			if p.QueuePosition == nil || *p.QueuePosition != len(all) {
				t.Fatal("wrong display position", p)
			}
			all = append(all, p.Number)
			if p.ParentPullNumber != 0 {
				parents[p.Number] = p.ParentPullNumber
			}
		}
		if !page.HasMore {
			break
		}
		cursor = "&cursor=" + url.QueryEscape(page.Cursor)
	}
	if want := []int64{3433, 3430, 3417, 3427, 3408, 3410, 3424, 3425, 3435, 3438}; !slices.Equal(all, want) {
		t.Fatalf("ordered %v want %v", all, want)
	}
	if parents[3410] != 3408 || parents[3424] != 3427 || parents[3435] != 3408 || parents[3438] != 0 || parents[3417] != 0 {
		t.Fatal("invalid parent display", parents)
	}
}

func TestQueueTopologyKeepsUnknownAndForksConservative(t *testing.T) {
	ready := false
	node := func(n, repo int64, head, base string) queueNode {
		return queueNode{number: n, state: "open", base: base, metadata: PullDisplayMetadata{IsDraft: &ready, Branches: &PullBranches{HeadRepositoryID: repo, HeadRef: head, DeliveryRef: "develop"}}}
	}
	fork := node(3, 22, "same", "develop")
	root := node(1, 7, "same", "develop")
	child := node(2, 7, "child", "same")
	order, parents, ok := topologyOrders([]queueNode{root, child, fork}, 7)
	if !ok || parents[2] != 1 || order[0].number != 3 {
		t.Fatal(order, parents, ok)
	}
	if _, _, ok = topologyOrders([]queueNode{child, fork}, 7); ok {
		t.Fatal("fork branch treated as same-repository parent")
	}
	missing := root
	missing.metadata.Branches = nil
	if _, _, ok = topologyOrders([]queueNode{missing, child, fork}, 7); ok {
		t.Fatal("missing facts promoted a standalone")
	}
}
