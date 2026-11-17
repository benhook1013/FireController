package reviewexec

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/adoptionholds"
	m "github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

func routedViewGet(t *testing.T, f *attemptFixture, query string) RoutedFindingsView {
	t.Helper()
	w := f.request("GET", "/api/v1/projects/"+attemptProject+"/reviews/routed-findings"+query, f.reader, "", "")
	var value RoutedFindingsView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &value) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if value.ProjectID != attemptProject || value.Items == nil {
		t.Fatal("missing scoped feed", value)
	}
	return value
}

func routedViewImport(t *testing.T, f *attemptFixture, project, namespace string, obligations []m.Obligation) (core.Scope, map[string]string) {
	t.Helper()
	scope := obligationSourceScope(t, f, project, "routed-importer-"+namespace)
	repository := int64(801)
	if project == otherAttemptProject {
		repository = 802
	}
	batch := m.Batch{Version: 1, Source: m.Source{Namespace: namespace, RepositoryID: repository, SnapshotSHA256: strings.Repeat("e", 64)}, ExpectedRevision: adjudicationRevision(0), Obligations: obligations}
	service := adoptionholds.New(f.store)
	preview, err := service.Preview(t.Context(), scope, batch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Apply(t.Context(), scope, "routed-import-"+namespace, m.ApplyRequest{Batch: batch, PreviewSHA256: preview.PreviewSHA256}); err != nil {
		t.Fatal(err)
	}
	page, err := service.List(t.Context(), scope, 0, "all", 100, "")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, item := range page.Items {
		ids[item.SourceKey] = item.ID
	}
	return scope, ids
}

func routedViewPublish(t *testing.T, f *attemptFixture, revision int64, targetState string) {
	t.Helper()
	request := reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 801, Source: "routed-view-fixture", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{projectionPull(2), projectionPull(4), projectionPull(6)}}
	request.Queue[1].State = targetState
	request.Queue[2].State = "closed"
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, fmt.Sprintf("routed-view-state-%d", revision), encoded(request))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestRoutedFindingsViewCombinesNativeAndCarriedWithoutInventedSource(t *testing.T) {
	f, a, evidence := routeFixture(t, 4)
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	reviews.Register(mux, f.store)
	f.handler = mux
	routedViewPublish(t, f, 0, "open")
	routes := make([]findingRouteReceipt, 4)
	for i := range routes {
		target := int64(4)
		if i%2 == 1 {
			target = 6
		}
		routes[i] = routeCreate(t, f, evidence, fmt.Sprintf("routed-view-native-%d", i), routeRequest(int64(i), int64(i), target))
	}
	routeDecision(t, f, routes[1].ID, "routed-view-accept", 1, "accepted")
	routeDecision(t, f, routes[2].ID, "routed-view-fixed-accept", 1, "accepted")
	routeFix(t, f, routes[2].ID, "routed-view-fix", 2, "", strings.Repeat("a", 40))
	routeDecision(t, f, routes[3].ID, "routed-view-reject", 1, "rejected")
	obligation := func(key, target string, affected ...int64) m.Obligation {
		return m.Obligation{SourceKey: key, Kind: "route", AffectedPullNumbers: affected, Summary: "  Original " + key + " title\nPrivate long detail not a list title", Evidence: []string{"target-pr:" + target}, OriginalStatus: "open"}
	}
	unknown := obligation("unknown", "unassigned", 4)
	unknown.Evidence = []string{"source:retained-no-target"}
	items := []m.Obligation{obligation("unassigned", "unassigned", 2), obligation("closed", "6", 2), obligation("open", "4", 2), obligation("resolved", "unassigned", 2), obligation("target-only", "6", 6), unknown, {SourceKey: "not-route", Kind: "finding", AffectedPullNumbers: []int64{2}, Summary: "Not a routed finding", Evidence: []string{}, OriginalStatus: "open"}}
	scope, ids := routedViewImport(t, f, attemptProject, "feed", items)
	_, err := adoptionholds.New(f.store).Resolve(t.Context(), scope, ids["resolved"], "routed-view-resolve", m.ResolveRequest{ExpectedRevision: adjudicationRevision(1), Reason: "Original carried work resolved", Evidence: []string{"proof:resolved"}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = routedViewImport(t, f, otherAttemptProject, "foreign", []m.Obligation{obligation("foreign", "unassigned", 2)})
	before := routeState(t, f, a)
	attention := routedViewGet(t, f, "?view=attention")
	if attention.View != "attention" || attention.PullNumber != 0 || attention.Counts != (RoutedFindingCounts{Open: 5}) || len(attention.Items) != 5 {
		t.Fatal("attention included assigned-open/resolved work or lost unfinished work", attention)
	}
	seen := map[string]RoutedFindingItem{}
	for _, item := range attention.Items {
		seen[item.ID] = item
		if item.State != "open" || strings.Contains(item.Title, "Private") || strings.Contains(item.Title, "\n") {
			t.Fatal(item)
		}
	}
	if seen[routes[1].ID].TargetState != "closed" || !reflect.DeepEqual(seen[routes[1].ID].SourcePullNumbers, []int64{2}) || seen[routes[1].ID].SourceEvidenceID != evidence || seen[routes[1].ID].Title != "Immutable finding 1" {
		t.Fatal("native source/title binding lost", seen[routes[1].ID])
	}
	if seen[ids["unassigned"]].TargetState != "unassigned" || seen[ids["unknown"]].TargetState != "unknown" || len(seen[ids["target-only"]].SourcePullNumbers) != 0 || seen[ids["target-only"]].SourcePullNumbers == nil || seen[ids["target-only"]].TargetPullNumber != 6 {
		t.Fatal("carried pointer invented source/lifecycle", seen)
	}
	pull := routedViewGet(t, f, "?pull_number=2")
	if pull.View != "pull" || pull.PullNumber != 2 || pull.Counts != (RoutedFindingCounts{Open: 5, Fixed: 1, Rejected: 1, Resolved: 1}) || len(pull.Items) != 8 {
		t.Fatal("full per-PR counts collapsed native/carried dispositions", pull)
	}
	// Explicit receiving pointer is retained even when only another affected PR
	// was imported; a target-only affected record never becomes a source claim.
	receiving := routedViewGet(t, f, "?pull_number=6")
	if receiving.Counts != (RoutedFindingCounts{Open: 3, Rejected: 1}) {
		t.Fatal("receiving carried target lost", receiving)
	}
	if routeState(t, f, a) != before {
		t.Fatal("feed read changed evidence/history/receipts")
	}
	// Current retained PR lifecycle, independently of execution Queue, changes
	// attention. No route or immutable source is rewritten by that publication.
	routedViewPublish(t, f, 1, "merged")
	attention = routedViewGet(t, f, "")
	if attention.Counts.Open != 7 {
		t.Fatal("merged target not surfaced", attention)
	}
	for _, item := range attention.Items {
		if item.TargetPullNumber == 4 && item.TargetState != "merged" {
			t.Fatal(item)
		}
	}
	routeFix(t, f, routes[1].ID, "routed-view-finish", 2, "", strings.Repeat("b", 40))
	if got := routedViewGet(t, f, ""); got.Counts.Open != 6 {
		t.Fatal("receiving fix did not leave attention", got)
	}
	// Supersession remains excluded; its replacement retains one source finding.
	reroute := routeRequest(4, 0, 6)
	reroute.ExpectedRouteRevision = 1
	routeCreate(t, f, evidence, "routed-view-reroute", reroute)
	if got := routedViewGet(t, f, "?pull_number=2"); got.Counts != (RoutedFindingCounts{Open: 4, Fixed: 2, Rejected: 1, Resolved: 1}) || len(got.Items) != 8 {
		t.Fatal("superseded route doubled inventory", got)
	}
}

func TestRoutedFindingsViewPagingAuthorizationAndStrictSelectors(t *testing.T) {
	f, _, evidence := routeFixture(t, 3)
	for i := range 3 {
		routeCreate(t, f, evidence, fmt.Sprintf("routed-page-%d", i), routeRequest(int64(i), int64(i), 4))
	}
	base := "/api/v1/projects/" + attemptProject + "/reviews/routed-findings"
	first := routedViewGet(t, f, "?pull_number=2&limit=1")
	if first.Counts.Open != 3 || !first.HasMore || first.Cursor == "" || first.Items[0].TargetState != "unknown" {
		t.Fatal(first)
	}
	seen := map[string]bool{first.Items[0].ID: true}
	cursor := first.Cursor
	for cursor != "" {
		page := routedViewGet(t, f, "?pull_number=2&limit=1&cursor="+url.QueryEscape(cursor))
		if page.Counts != first.Counts || len(page.Items) != 1 || seen[page.Items[0].ID] {
			t.Fatal("counts became page-relative or replayed item", page)
		}
		seen[page.Items[0].ID] = true
		cursor = page.Cursor
	}
	if len(seen) != 3 {
		t.Fatal(seen)
	}
	for _, query := range []string{"?pull_number=0", "?pull_number=02", "?pull_number=2&view=attention", "?view=pull", "?view=", "?limit=0", "?limit=101", "?pull_number=2&pull_number=2", "?unknown=true", "?pull_number=4&cursor=" + url.QueryEscape(first.Cursor)} {
		evidenceHTTPRequire(t, f.request("GET", base+query, f.reader, "", ""), 400, "validation_failed")
	}
	evidenceHTTPRequire(t, f.request("GET", base, "", "", ""), 401, "unauthorized")
	evidenceHTTPRequire(t, f.request("GET", base, f.other, "", ""), 403, "forbidden")
	other := "/api/v1/projects/" + otherAttemptProject + "/reviews/routed-findings"
	evidenceHTTPRequire(t, f.request("GET", other+"?pull_number=2&cursor="+url.QueryEscape(first.Cursor), f.other, "", ""), 400, "validation_failed")
	w := f.request("GET", other, f.other, "", "")
	var empty RoutedFindingsView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &empty) != nil || len(empty.Items) != 0 || empty.Counts != (RoutedFindingCounts{}) {
		t.Fatal("cross-project leak", w.Code, w.Body.String())
	}
	// A malformed current/source link cannot be repackaged as readable evidence.
	if _, err := f.store.DB().Exec(`UPDATE review_finding_routes SET source_pull_number=99 WHERE id=?`, first.Items[0].ID); err != nil {
		t.Fatal(err)
	}
	evidenceHTTPRequire(t, f.request("GET", base+"?pull_number=4", f.reader, "", ""), 500, "storage_unavailable")
}

func TestRoutedFindingsViewBoundedPointersTitlesAndHistoryOutsideQueue(t *testing.T) {
	for _, refs := range [][]string{{}, {"target-pr:01"}, {"target-pr:2147483648"}, {"target-pr:6", "target-pr:unassigned"}, {"target-pr:6", "target-pr:6"}} {
		if routedCarriedTarget(refs) != -1 {
			t.Fatal("ambiguous pointer became assigned", refs)
		}
	}
	if routedCarriedTarget([]string{"target-pr:unassigned"}) != 0 || routedCarriedTarget([]string{"target-pr:6"}) != 6 {
		t.Fatal("canonical pointer lost")
	}
	title := routedFindingTitle("\n" + strings.Repeat("界", 1000) + "\nprivate detail")
	if len(title) > 512 || !strings.HasSuffix(title, "…") {
		t.Fatal("title not bounded", len(title))
	}
	f, _, evidence := routeFixture(t, 1)
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	reviews.Register(mux, f.store)
	f.handler = mux
	routedViewPublish(t, f, 0, "merged")
	routeCreate(t, f, evidence, "outside-queue-route", routeRequest(0, 0, 4))
	// Remove the target from execution configuration without changing the
	// canonical retained merged PR. Attention must not rely on that queue.
	previous := int64(2)
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "outside-queue", Configuration{ExpectedRevision: &previous, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 6}, Channels: []string{"cli", "hosted"}}); err != nil {
		t.Fatal(err)
	}
	if got := routedViewGet(t, f, ""); got.Counts.Open != 1 || got.Items[0].TargetState != "merged" {
		t.Fatal("queue-window omission hid merged target", got)
	}
}
