package reviews

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
	"time"
)

func componentNode(n int64, head, base string) queueNode {
	return queueNode{number: n, state: "open", base: base, metadata: PullDisplayMetadata{Branches: &PullBranches{HeadRepositoryID: 701, HeadRef: head, DeliveryRef: "develop"}}}
}
func componentNumbers(nodes []queueNode) []int64 {
	out := []int64{}
	for _, n := range nodes {
		out = append(out, n.number)
	}
	return out
}
func TestTopologyDraftIsNotOrderingAuthority(t *testing.T) {
	nodes := []queueNode{componentNode(2, "child", "root"), componentNode(1, "root", "develop"), componentNode(3, "independent", "develop"), componentNode(4, "sibling", "root")}
	// Nil draft truth does not erase otherwise proven branch topology.
	for _, draft := range []*bool{nil, new(true), new(false)} {
		for i := range nodes {
			nodes[i].metadata.IsDraft = draft
		}
		got, parents, complete := topologyOrders(nodes, 701)
		if !complete || !slices.Equal(componentNumbers(got), []int64{3, 1, 2, 4}) || parents[2] != 1 || parents[4] != 1 {
			t.Fatal(got, parents, complete)
		}
	}
	nodes[1].metadata.IsDraft = new(true)
	nodes[0].metadata.IsDraft = new(false)
	got, _, _ := topologyOrders(nodes, 701)
	if !slices.Equal(componentNumbers(got), []int64{3, 1, 2, 4}) {
		t.Fatal("draft parent moved behind ready child", got)
	}
}
func TestTopologyUnresolvedComponentsKeepOnlyTheirSlots(t *testing.T) {
	for _, kind := range []string{"missing-parent", "missing-metadata", "cycle", "ambiguous", "self", "closed-parent", "fork-parent", "unknown", "conflicting-delivery"} {
		t.Run(kind, func(t *testing.T) {
			root := componentNode(1, "root", "develop")
			child := componentNode(2, "child", "root")
			independent := componentNode(3, "independent", "develop")
			unresolved := componentNode(9, "unresolved", "absent")
			nodes := []queueNode{child, unresolved, root, independent}
			switch kind {
			case "missing-metadata":
				nodes[1].metadata.Branches = nil
			case "cycle":
				nodes[1].base = "cycle"
				nodes = append(nodes, componentNode(10, "cycle", "unresolved"))
			case "ambiguous":
				nodes[1].base = "duplicate"
				nodes = append(nodes, componentNode(10, "duplicate", "develop"), componentNode(11, "duplicate", "develop"))
			case "self":
				nodes[1].base = "unresolved"
			case "closed-parent":
				nodes = append(nodes, componentNode(10, "absent", "develop"))
				nodes[4].state = "closed"
			case "fork-parent":
				nodes = append(nodes, componentNode(10, "absent", "unresolved"))
				nodes[4].metadata.Branches.HeadRepositoryID = 702
			case "unknown":
				nodes[1].state = "unknown"
			case "conflicting-delivery":
				nodes[1].base = "branch"
				nodes = append(nodes, componentNode(10, "branch", "develop"))
				nodes[1].metadata.Branches.DeliveryRef = "other"
			}
			before := slices.Clone(nodes)
			got, _, complete := topologyOrders(nodes, 701)
			want := []int64{3, 9, 1, 2}
			if kind == "conflicting-delivery" {
				want = componentNumbers(before[:4])
			}
			if complete || !slices.Equal(componentNumbers(got[:4]), want) {
				t.Fatal(got, complete)
			}
			for i := 4; i < len(nodes); i++ {
				if got[i].number != nodes[i].number {
					t.Fatal("unresolved slot moved", got)
				}
			}
			if !reflect.DeepEqual(nodes, before) {
				t.Fatal("input mutated")
			}
			again, _, _ := topologyOrders(got, 701)
			if !reflect.DeepEqual(got, again) {
				t.Fatal("unstable repeat", got, again)
			}
		})
	}
}
func TestTopologyIncompleteChildDoesNotInventIndependentParent(t *testing.T) {
	root := componentNode(1, "root", "develop")
	child := componentNode(2, "child", "root")
	child.metadata.Branches = nil
	nodes := []queueNode{root, child, componentNode(4, "stack-child", "stack"), componentNode(5, "stack", "develop"), componentNode(6, "standalone", "develop")}
	got, parents, complete := topologyOrders(nodes, 701)
	if complete || !slices.Equal(componentNumbers(got), []int64{1, 2, 6, 5, 4}) || parents[2] != 1 {
		t.Fatal(got, parents, complete)
	}
}
func TestRecentTopologyUnknownDoesNotFreezeIndependentComponents(t *testing.T) {
	nodes := []queueNode{componentNode(2, "child", "root"), {number: 9, state: "unknown"}, componentNode(1, "root", "develop"), componentNode(3, "standalone", "develop")}
	got, parents := orderedRecentNodes(nodes, 701, time.Now())
	if !slices.Equal(componentNumbers(got), []int64{3, 9, 1, 2}) || parents[2] != 1 {
		t.Fatal(got, parents)
	}
}
func TestExecutionTopologyUsesOnlyConfiguredMembership(t *testing.T) {
	f := newAPIFixture(t)
	nodes := []queueNode{componentNode(2, "child", "root"), componentNode(9, "external-child", "unenrolled"), componentNode(1, "root", "develop"), componentNode(3, "independent", "develop"), componentNode(99, "historical", "unknown")}
	pulls := []PullInput{}
	for _, n := range nodes {
		p := samplePull(n.number, n.metadata.Branches.HeadRef)
		p.State = "open"
		p.BaseRef = n.base
		pulls = append(pulls, p)
	}
	f.observe(t, reviewRequest(0, pulls), "component-order")
	for _, n := range nodes {
		raw, _ := json.Marshal(n.metadata)
		if _, err := f.store.DB().Exec(`UPDATE review_pull_versions SET metadata_json=? WHERE project_id=? AND number=?`, string(raw), f.project, n.number); err != nil {
			t.Fatal(err)
		}
	}
	// Configured missing row 88 retains its slot; unenrolled historical99 cannot
	// invalidate the independent components or become a dependency by position.
	configured := []int64{2, 9, 88, 1, 3}
	tx, err := f.store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	got, err := OrderedExecutionQueue(t.Context(), tx, f.project, configured)
	if err != nil || !slices.Equal(got, []int64{3, 9, 88, 1, 2}) {
		t.Fatal(got, err)
	}
	if !slices.Equal(configured, []int64{2, 9, 88, 1, 3}) {
		t.Fatal("configured order rewritten")
	}
}

func TestTopologyUnknownChildKeepsConnectedParentUnresolved(t *testing.T) {
	root := componentNode(1, "root", "develop")
	child := componentNode(2, "child", "root")
	child.state = "unknown"
	nodes := []queueNode{root, child, componentNode(4, "stack-child", "stack"), componentNode(5, "stack", "develop"), componentNode(6, "standalone", "develop")}
	got, _, complete := topologyOrders(nodes, 701)
	if complete || !slices.Equal(componentNumbers(got), []int64{1, 2, 6, 5, 4}) {
		t.Fatal(got, complete)
	}
}

func TestTopologyDisconnectedConflictingDeliveryRefsRemainUncertain(t *testing.T) {
	nodes := []queueNode{componentNode(2, "child", "root"), componentNode(1, "root", "develop"), componentNode(3, "other", "main")}
	nodes[2].metadata.Branches.DeliveryRef = "main"
	got, _, complete := topologyOrders(nodes, 701)
	if complete || !slices.Equal(componentNumbers(got), componentNumbers(nodes)) {
		t.Fatal("conflicting roots promoted", got, complete)
	}
}

func shortenedStackNodes() []queueNode {
	nodes := []queueNode{componentNode(83, "old-root", "main"), componentNode(84, "remaining-root", "old-root"), componentNode(102, "child", "remaining-root"), componentNode(87, "tip", "child"), componentNode(45, "draft", "main"), componentNode(108, "review-closed", "main")}
	for i := range nodes {
		nodes[i].metadata.Branches.DeliveryRef = "main"
	}
	nodes[0].state = "merged"
	nodes[0].metadata.MergedAt = "2020-01-01T00:00:00Z"
	nodes[4].metadata.IsDraft = new(true)
	return nodes
}

func TestTopologyMergedParentAnchorsOnlyShortenedStackOrder(t *testing.T) {
	for _, soleChild := range []bool{false, true} {
		t.Run(map[bool]string{false: "stack", true: "sole-child"}[soleChild], func(t *testing.T) {
			nodes := shortenedStackNodes()
			want := []int64{83, 45, 108, 84, 102, 87}
			wantParents := map[int64]int64{102: 84, 87: 102}
			if soleChild {
				nodes = append(nodes[:2], nodes[4:]...)
				want = []int64{83, 45, 108, 84}
				wantParents = map[int64]int64{}
			}
			before, _ := json.Marshal(nodes)
			got, parents, _ := topologyOrders(nodes, 701)
			if !slices.Equal(componentNumbers(got), want) || !reflect.DeepEqual(parents, wantParents) {
				t.Fatalf("execution order %v parents %v; want %v parents %v", componentNumbers(got), parents, want, wantParents)
			}
			for _, mode := range []string{"auto", "manual"} {
				recent, display := orderedRecentNodesMode(nodes, 701, time.Now(), mode)
				expected := want
				if mode == "manual" {
					expected = componentNumbers(nodes)
				}
				if !slices.Equal(componentNumbers(recent), expected) || !reflect.DeepEqual(display, wantParents) {
					t.Fatalf("%s browser order %v parents %v", mode, componentNumbers(recent), display)
				}
			}
			after, _ := json.Marshal(nodes)
			if string(before) != string(after) || nodes[1].base != "old-root" {
				t.Fatal("sorting changed retained metadata/base")
			}
			again, _, _ := topologyOrders(got, 701)
			if !reflect.DeepEqual(got, again) {
				t.Fatal("unstable shortened stack order")
			}
		})
	}
}

func TestTopologyMergedAnchorRequiresUniqueMatchingIdentity(t *testing.T) {
	for _, kind := range []string{"absent", "closed", "unknown", "fork", "missing-metadata", "missing-delivery", "stale-delivery", "reused-merged", "reused-closed", "reused-unknown"} {
		t.Run(kind, func(t *testing.T) {
			nodes := shortenedStackNodes()
			switch kind {
			case "absent":
				nodes = nodes[1:]
			case "closed", "unknown":
				nodes[0].state = kind
				nodes[0].metadata.MergedAt = ""
			case "fork":
				nodes[0].metadata.Branches.HeadRepositoryID = 702
			case "missing-metadata":
				nodes[0].metadata.Branches = nil
			case "missing-delivery":
				nodes[0].metadata.Branches.DeliveryRef = ""
			case "stale-delivery":
				nodes[0].metadata.Branches.DeliveryRef = "develop"
			default:
				duplicate := componentNode(90, "old-root", "main")
				duplicate.metadata.Branches.DeliveryRef = "main"
				duplicate.state = map[string]string{"reused-merged": "merged", "reused-closed": "closed", "reused-unknown": "unknown"}[kind]
				nodes = append(nodes, duplicate)
			}
			got, parents, complete := topologyOrders(nodes, 701)
			if complete || !slices.Equal(componentNumbers(got), componentNumbers(nodes)) || parents[84] != 0 {
				t.Fatalf("%s incorrectly anchored: %v %v %v", kind, componentNumbers(got), parents, complete)
			}
		})
	}
}

func TestTopologyStaleTerminalDeliveryDoesNotFreezeActiveComponents(t *testing.T) {
	nodes := []queueNode{componentNode(2, "child", "root"), componentNode(90, "historical", "main"), componentNode(1, "root", "develop"), componentNode(3, "standalone", "develop")}
	for _, state := range []string{"merged", "closed"} {
		nodes[1].state = state
		nodes[1].metadata.Branches.DeliveryRef = "main"
		got, _, _ := topologyOrders(nodes, 701)
		if !slices.Equal(componentNumbers(got), []int64{3, 90, 1, 2}) {
			t.Fatalf("%s stale terminal delivery froze active order: %v", state, componentNumbers(got))
		}
		nodes[1].metadata.Branches.DeliveryRef = "root"
		if parents := displayParents(nodes, 701); parents[2] != 1 {
			t.Fatalf("%s stale terminal delivery hid the active parent: %v", state, parents)
		}
	}
}

func TestRecentTopologyMergedAnchorOutsideRecentHistory(t *testing.T) {
	nodes := shortenedStackNodes()
	for i, number := range []int64{91, 92} {
		n := componentNode(number, "newer-merge", "develop")
		n.state = "merged"
		n.metadata.MergedAt = time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
		nodes = append(nodes, n)
	}
	got, parents := orderedRecentNodes(nodes, 701, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	if !slices.Equal(componentNumbers(got), []int64{92, 91, 45, 108, 84, 102, 87}) || !reflect.DeepEqual(parents, map[int64]int64{102: 84, 87: 102}) {
		t.Fatalf("old merged anchor lost when excluded from recent history: %v %v", componentNumbers(got), parents)
	}
	if !slices.Equal(componentNumbers(mergedQueueNodes(nodes)), []int64{92, 91, 83}) {
		t.Fatal("merged history lost its anchor row")
	}
}

func TestExecutionTopologyMergedParentRetainsConfiguredSlot(t *testing.T) {
	f := newAPIFixture(t)
	nodes := shortenedStackNodes()
	// History can occupy any configured slot, not only a merged prefix.
	nodes[0], nodes[1] = nodes[1], nodes[0]
	pulls := []PullInput{}
	for _, n := range nodes {
		p := samplePull(n.number, n.metadata.Branches.HeadRef)
		p.State, p.BaseRef = n.state, n.base
		p.IsDraft, p.MergedAt = n.metadata.IsDraft, n.metadata.MergedAt
		pulls = append(pulls, p)
	}
	f.observe(t, reviewRequest(0, pulls), "shortened-stack")
	for _, n := range nodes {
		raw, err := json.Marshal(n.metadata)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.DB().Exec(`UPDATE review_pull_versions SET metadata_json=? WHERE project_id=? AND number=?`, string(raw), f.project, n.number); err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews/queue?view=recent&limit=100", nil)
	r.Header.Set("Authorization", "Bearer "+f.readToken)
	r.Header.Set("X-FC-Queue-Topology", "v1")
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	var page QueueResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil {
		t.Fatalf("browser queue: %d %s", w.Code, w.Body.String())
	}
	var browserOrder []int64
	parents := map[int64]int64{}
	for _, p := range page.Items {
		browserOrder = append(browserOrder, p.Number)
		if p.ParentPullNumber != 0 {
			parents[p.Number] = p.ParentPullNumber
		}
		if p.Number == 84 && p.BaseRef != "old-root" {
			t.Fatalf("browser rewrote retained base: %q", p.BaseRef)
		}
	}
	if !slices.Equal(browserOrder, []int64{83, 45, 108, 84, 102, 87}) || !reflect.DeepEqual(parents, map[int64]int64{102: 84, 87: 102}) {
		t.Fatalf("browser order %v parents %v", browserOrder, parents)
	}
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	configured := componentNumbers(nodes)
	got, err := OrderedExecutionQueue(t.Context(), tx, f.project, configured)
	if err != nil || !slices.Equal(got, []int64{45, 83, 108, 84, 102, 87}) {
		t.Fatalf("execution order %v: %v", got, err)
	}
	if !slices.Equal(configured, componentNumbers(nodes)) {
		t.Fatal("configured membership/order changed")
	}
	var base, state string
	if err := tx.QueryRow(`SELECT base_ref,state FROM review_pull_versions WHERE project_id=? AND number=84`, f.project).Scan(&base, &state); err != nil || base != "old-root" || state != "open" {
		t.Fatalf("stored child changed: %q %q %v", base, state, err)
	}
}
