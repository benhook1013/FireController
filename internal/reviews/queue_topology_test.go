package reviews

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

func setTopologyBranches(t *testing.T, f *apiFixture, branches map[int64]PullBranches) {
	t.Helper()
	for number, value := range branches {
		var state, raw string
		if err := f.store.DB().QueryRow(`SELECT state,metadata_json FROM review_pull_versions WHERE project_id=? AND number=?`, f.project, number).Scan(&state, &raw); err != nil {
			t.Fatal(err)
		}
		metadata, err := ReadPullMetadata(raw, state)
		if err != nil {
			t.Fatal(err)
		}
		metadata.Branches = &value
		body, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.store.DB().Exec(`UPDATE review_pull_versions SET metadata_json=? WHERE project_id=? AND number=?`, string(body), f.project, number); err != nil {
			t.Fatal(err)
		}
	}
}

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
	if want := []int64{3433, 3430, 3417, 3427, 3408, 3410, 3438, 3435, 3424, 3425}; !slices.Equal(all, want) {
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

func TestParentBaseMovedProjectionUsesFullSnapshotAndPreservesOtherFacts(t *testing.T) {
	f := newAPIFixture(t)
	sha := func(c byte) string { return strings.Repeat(string(c), 40) }
	child := samplePull(2, "moved child")
	child.State, child.HeadSHA, child.BaseRef, child.BaseSHA, child.CIState = "open", sha('b'), "root", sha('c'), "failure"
	child.MergeStateStatus = "CLEAN"
	aligned := samplePull(3, "aligned child")
	aligned.State, aligned.HeadSHA, aligned.BaseRef, aligned.BaseSHA = "open", sha('e'), "root", sha('a')
	parent := samplePull(1, "parent outside first page")
	parent.State, parent.HeadSHA, parent.BaseRef, parent.BaseSHA = "open", sha('a'), "develop", sha('d')
	f.observe(t, reviewRequest(0, []PullInput{child, aligned, parent}), "parent-base-projection")
	setTopologyBranches(t, f, map[int64]PullBranches{
		2: {HeadRepositoryID: 701, HeadRef: "child", DeliveryRef: "develop"},
		3: {HeadRepositoryID: 701, HeadRef: "aligned", DeliveryRef: "develop"},
		1: {HeadRepositoryID: 701, HeadRef: "root", DeliveryRef: "develop"},
	})

	path := "/api/v1/projects/" + f.project + "/reviews/queue?view=recent&limit=1&include_merge_status=true"
	w := f.call(http.MethodGet, path, f.readToken, "", nil)
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 {
		t.Fatalf("projected queue: %d %s", w.Code, w.Body.String())
	}
	var moved map[string]json.RawMessage
	if err := json.Unmarshal(page.Items[0], &moved); err != nil {
		t.Fatal(err)
	}
	var movedValue bool
	if err := json.Unmarshal(moved["parent_base_moved"], &movedValue); err != nil || !movedValue || string(moved["number"]) != "2" {
		t.Fatalf("parent outside the loaded page did not prove movement: %s", page.Items[0])
	}
	if string(moved["ci_state"]) != `"failure"` || !bytes.Contains(page.Items[0], []byte(`"merge_status":{"state":"ready"`)) {
		t.Fatalf("parent movement overwrote independent CI or GitHub facts: %s", page.Items[0])
	}
	full := f.call(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews/queue?limit=1&include_merge_status=true", f.readToken, "", nil)
	var fullPage struct {
		Items []json.RawMessage `json:"items"`
	}
	if full.Code != http.StatusOK || json.Unmarshal(full.Body.Bytes(), &fullPage) != nil || len(fullPage.Items) != 1 || !bytes.Contains(fullPage.Items[0], []byte(`"parent_base_moved":true`)) {
		t.Fatalf("all-queue projection depended on its loaded page: %d %s", full.Code, full.Body.String())
	}

	all := f.call(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews/queue?view=recent&limit=10&include_merge_status=true", f.readToken, "", nil)
	var allPage struct {
		Items []json.RawMessage `json:"items"`
	}
	if all.Code != http.StatusOK || json.Unmarshal(all.Body.Bytes(), &allPage) != nil {
		t.Fatalf("full projected queue: %d %s", all.Code, all.Body.String())
	}
	for _, raw := range allPage.Items {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatal(err)
		}
		if string(item["number"]) == "3" && len(item["parent_base_moved"]) != 0 {
			t.Fatalf("aligned base emitted a false hold: %s", raw)
		}
	}
	detail := f.call(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews/pulls/2?include_merge_status=true", f.readToken, "", nil)
	var pull struct {
		Pull PullSummary `json:"pull"`
	}
	if detail.Code != http.StatusOK || json.Unmarshal(detail.Body.Bytes(), &pull) != nil || !pull.Pull.ParentBaseMoved {
		t.Fatalf("detail omitted same-snapshot movement: %d %s", detail.Code, detail.Body.String())
	}
	legacy := f.call(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews/queue?view=recent&limit=1", f.readToken, "", nil)
	if legacy.Code != http.StatusOK || bytes.Contains(legacy.Body.Bytes(), []byte(`"parent_base_moved"`)) || bytes.Contains(legacy.Body.Bytes(), []byte(`"merge_status"`)) {
		t.Fatalf("unopted response changed its native field set: %d %s", legacy.Code, legacy.Body.String())
	}
}

func TestParentBaseMovedRejectsUnprovenRelationshipsAndIdentities(t *testing.T) {
	f := newAPIFixture(t)
	sha := func(c byte) string { return fmt.Sprintf("%040x", c) }
	type row struct {
		number            int64
		state, head, base string
		headSHA, baseSHA  string
		headRepositoryID  int64
	}
	rows := []row{
		{1, "open", "root", "develop", sha('a'), sha('d'), 701},
		{2, "open", "duplicate", "develop", sha('b'), sha('d'), 701},
		{3, "open", "duplicate", "develop", sha('c'), sha('d'), 701},
		{4, "open", "ambiguous-child", "duplicate", sha('e'), sha('f'), 701},
		{5, "open", "fork-parent", "develop", sha('g'), sha('d'), 702},
		{6, "open", "fork-child", "fork-parent", sha('h'), sha('i'), 701},
		{7, "open", "foreign-child", "root", sha('j'), sha('k'), 702},
		{8, "closed", "terminal-parent", "develop", sha('l'), sha('d'), 701},
		{9, "open", "terminal-child", "terminal-parent", sha('m'), sha('n'), 701},
		{10, "unknown", "unknown-child", "root", sha('o'), sha('p'), 701},
		{11, "open", "self", "self", sha('q'), sha('r'), 701},
		{12, "open", "empty-base-child", "root", sha('s'), "", 701},
		{13, "open", "empty-head-child", "root", "", sha('t'), 701},
		{14, "open", "missing-sha-parent", "develop", "", sha('d'), 701},
		{15, "open", "missing-sha-child", "missing-sha-parent", sha('u'), sha('v'), 701},
	}
	pulls := make([]PullInput, 0, len(rows))
	branches := make(map[int64]PullBranches, len(rows))
	for _, item := range rows {
		pull := samplePull(item.number, "conservative parent proof")
		pull.State, pull.HeadSHA, pull.BaseRef, pull.BaseSHA = item.state, item.headSHA, item.base, item.baseSHA
		pulls = append(pulls, pull)
		branches[item.number] = PullBranches{HeadRepositoryID: item.headRepositoryID, HeadRef: item.head, DeliveryRef: "develop"}
	}
	f.observe(t, reviewRequest(0, pulls), "parent-base-conservative")
	setTopologyBranches(t, f, branches)
	w := f.call(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews/queue?include_merge_status=true&limit=100", f.readToken, "", nil)
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil {
		t.Fatalf("conservative queue: %d %s", w.Code, w.Body.String())
	}
	for _, raw := range page.Items {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatal(err)
		}
		if len(item["parent_base_moved"]) != 0 {
			t.Fatalf("unproven relationship or incomplete SHA asserted a hold: %s", raw)
		}
	}
}
