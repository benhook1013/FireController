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
	for i, n := range []int64{3433, 3430} {
		p := samplePull(n, "merged")
		p.MergedAt = time.Now().UTC().Add(-time.Duration(i) * time.Second).Format(time.RFC3339Nano)
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
	r := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews/pulls/3410", nil)
	r.Header.Set("Authorization", "Bearer "+f.readToken)
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	var detail PullResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.Pull.ParentPullNumber != 3408 {
		t.Fatalf("detail lost parent: %d %s", w.Code, w.Body.String())
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

func TestParentDisplaySurvivesUnrelatedIncompleteTopology(t *testing.T) {
	draft := false
	nodes := []queueNode{
		{number: 3427, state: "open", base: "develop", metadata: PullDisplayMetadata{IsDraft: &draft, Branches: &PullBranches{HeadRepositoryID: 701, HeadRef: "codex/gameplay-region-owner-handoff-20261114", DeliveryRef: "develop"}}},
		{number: 3408, state: "open", base: "codex/gameplay-region-owner-handoff-20261114", metadata: PullDisplayMetadata{IsDraft: &draft}},
		{number: 9999, state: "unknown", base: "unknown"},
	}
	ordered, parents := orderedRecentNodes(nodes, 701, time.Now())
	if parents[3408] != 3427 {
		t.Fatalf("lost proven parent: %v", parents)
	}
	if ordered[0].number != 3427 || ordered[1].number != 3408 || ordered[2].number != 9999 {
		t.Fatal("changed fallback order", ordered)
	}
	nodes = append(nodes, queueNode{number: 123, state: "open", metadata: nodes[0].metadata})
	if p := displayParents(nodes, 701); p[3408] != 0 {
		t.Fatal("ambiguous parent", p)
	}
	if p := displayParents(nodes[:2], 702); p[3408] != 0 {
		t.Fatal("cross repository parent", p)
	}
}

func TestParentDisplayFallbackBoundaries(t *testing.T) {
	parent := queueNode{number: 1, state: "open", base: "develop", metadata: PullDisplayMetadata{Branches: &PullBranches{HeadRepositoryID: 7, HeadRef: "feature", DeliveryRef: "develop"}}}
	child := queueNode{number: 2, state: "open", base: "feature"}
	for _, state := range []string{"closed", "merged", "unknown"} {
		p := parent
		p.state = state
		if got := displayParents([]queueNode{p, child}, 7); got[2] != 0 {
			t.Fatal(state, got)
		}
	}
	self := parent
	self.base = "feature"
	if got := displayParents([]queueNode{self}, 7); got[1] != 0 {
		t.Fatal("self", got)
	}
	duplicate := parent
	duplicate.number = 3
	third := parent
	third.number = 4
	if got := displayParents([]queueNode{parent, duplicate, third, child}, 7); got[2] != 0 {
		t.Fatal("third duplicate", got)
	}
	parent.metadata.Branches.HeadRef = "develop"
	child.base = "develop"
	if got := displayParents([]queueNode{parent, child}, 7); got[2] != 0 {
		t.Fatal("delivery base with missing child identity", got)
	}
}
