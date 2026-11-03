package reviewexec

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/adoptionholds"
	m "github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestRoutedRetargetUsesCurrentReceiverForAttentionAndAdmission(t *testing.T) {
	f := newAttemptFixture(t)
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	reviews.Register(mux, f.store)
	f.handler = mux
	routedViewPublish(t, f, 0, "open")
	scope, ids := routedViewImport(t, f, attemptProject, "retarget-view", []m.Obligation{{SourceKey: "old-route", Kind: "route", AffectedPullNumbers: []int64{2, 6}, Summary: "Continuing correction", Evidence: []string{"target-pr:6", "proof:original"}, OriginalStatus: "open"}})
	service := adoptionholds.New(f.store)
	before, err := service.Detail(t.Context(), scope, ids["old-route"])
	if err != nil {
		t.Fatal(err)
	}
	if first := routedViewGet(t, f, "?view=attention"); first.Counts.Open != 1 || first.Items[0].TargetPullNumber != 6 {
		t.Fatal(first)
	}
	revision, target := int64(1), int64(4)
	if _, err := service.Retarget(t.Context(), scope, ids["old-route"], "move-open", m.RetargetRequest{ExpectedRevision: &revision, TargetPullNumber: &target, Reason: "Continue in open receiver"}); err != nil {
		t.Fatal(err)
	}
	if attention := routedViewGet(t, f, "?view=attention"); attention.Counts.Open != 0 || len(attention.Items) != 0 {
		t.Fatal("old closed marker remained current", attention)
	}
	receiving := routedViewGet(t, f, "?pull_number=4")
	if receiving.Counts.Open != 1 || len(receiving.Items) != 1 || receiving.Items[0].TargetPullNumber != 4 || receiving.Items[0].Revision != 2 {
		t.Fatal("receiving history lost current route", receiving)
	}
	for n, want := range map[int64]int64{2: 1, 4: 1, 6: 0} {
		facts, err := adoptionholds.ReadFacts(t.Context(), f.store.DB(), attemptProject, n)
		if err != nil || facts.Open != want {
			t.Fatal("admission scope differs from view", n, facts, err)
		}
	}
	after, err := service.Detail(t.Context(), scope, ids["old-route"])
	if err != nil || !reflect.DeepEqual(before.Original, after.Original) || after.Status != "open" {
		t.Fatal("retarget rewrote provenance or resolved work", after, err)
	}
	revision, target = 2, 0
	if _, err := service.Retarget(t.Context(), scope, ids["old-route"], "move-backlog", m.RetargetRequest{ExpectedRevision: &revision, TargetPullNumber: &target, Reason: "Keep outstanding work unassigned"}); err != nil {
		t.Fatal(err)
	}
	attention := routedViewGet(t, f, "?view=attention")
	if attention.Counts.Open != 1 || len(attention.Items) != 1 || attention.Items[0].TargetState != "unassigned" || attention.Items[0].TargetPullNumber != 0 {
		t.Fatal("unassigned route disappeared", attention)
	}
}
