package reviews

import (
	"context"
	"encoding/json"
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
	for _, kind := range []string{"missing-parent", "missing-metadata", "cycle", "ambiguous", "self", "terminal-parent", "fork-parent", "unknown", "conflicting-delivery"} {
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
			case "terminal-parent":
				nodes = append(nodes, componentNode(10, "absent", "develop"))
				nodes[4].state = "merged"
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
