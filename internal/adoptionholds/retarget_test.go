package adoptionholds_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	h "github.com/benhook1013/FireController/internal/adoptionholds"
	m "github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/core"
)

func retargetRoute(t *testing.T, f *fixture, targets []int64, evidence []string) string {
	t.Helper()
	b := batch()
	b.Obligations = []m.Obligation{{SourceKey: "route-1", Kind: "route", AffectedPullNumbers: targets, Evidence: evidence, Summary: "A carried route needs a current receiver", OriginalStatus: "open"}}
	f.apply(b, "retarget-seed")
	return f.page("?status=all").Items[0].ID
}

func retargetInput(revision, target int64) m.RetargetRequest {
	return m.RetargetRequest{ExpectedRevision: &revision, TargetPullNumber: &target, Reason: "Assign the remaining fix to its current PR"}
}

func retargetDetail(t *testing.T, f *fixture, id string) m.Detail {
	t.Helper()
	return decode[m.Detail](t, f.call("GET", base+"/"+id, f.token, "", "", nil), http.StatusOK)
}

func retainedRetargetSource(t *testing.T, f *fixture, id string) string {
	t.Helper()
	var original, checksum, body, batchChecksum string
	if err := f.store.DB().QueryRow(`SELECT o.original_json,o.original_sha256,i.batch_json,i.batch_sha256 FROM review_adoption_obligations o JOIN review_adoption_imports i ON i.project_id=o.project_id AND i.namespace=o.namespace AND i.revision=o.source_revision WHERE o.id=?`, id).Scan(&original, &checksum, &body, &batchChecksum); err != nil {
		t.Fatal(err)
	}
	return original + checksum + body + batchChecksum
}

func TestRouteRetargetMovesOnlyReceiverRetainsSourceAndResolvesAtCurrentRevision(t *testing.T) {
	f := fresh(t)
	id := retargetRoute(t, f, []int64{2898, 3105}, []string{"target-pr:3105"})
	source := retainedRetargetSource(t, f, id)
	original := retargetDetail(t, f, id)
	path := base + "/" + id + "/retarget"
	first := f.call("POST", path, f.token, "worker_one", "move-route", retargetInput(1, 3341))
	receipt := decode[m.RetargetReceipt](t, first, 200)
	if receipt.Revision != 2 || receipt.TargetPullNumber != 3341 || receipt.Outcome != "retargeted" || !reflect.DeepEqual(receipt.AffectedPullNumbers, []int64{2898, 3341}) {
		t.Fatal("wrong retarget result", receipt)
	}
	detail := retargetDetail(t, f, id)
	if detail.Retarget == nil || detail.Retarget.Revision != 2 || detail.Retarget.TargetPullNumber != 3341 || detail.Retarget.WorkerID != "worker_one" || !reflect.DeepEqual(detail.Original, original.Original) {
		t.Fatal("retarget attribution or original facts changed", detail)
	}
	for number, want := range map[int64]int64{2898: 1, 3105: 0, 3341: 1} {
		facts, err := h.ReadFacts(t.Context(), f.store.DB(), "project_one", number)
		if err != nil || facts.Open != want {
			t.Fatal("current admission scope did not follow receiver", number, facts, err)
		}
	}
	if len(f.page("?pull_number=3105").Items) != 0 || !reflect.DeepEqual(f.page("?pull_number=3341").Items[0].AffectedPullNumbers, []int64{2898, 3341}) {
		t.Fatal("list uses original receiver instead of current scope")
	}
	if _, err := f.store.DB().Exec(`DELETE FROM review_adoption_targets WHERE obligation_id=? AND pull_number=3341`, id); err == nil {
		t.Fatal("current receiver guard could be silently deleted after retarget")
	}
	unchanged := decode[m.RetargetReceipt](t, f.call("POST", path, f.token, "worker_one", "same-route", retargetInput(2, 3341)), 200)
	if unchanged.Revision != 2 || unchanged.Outcome != "unchanged" || f.count("review_adoption_retargets") != 1 {
		t.Fatal("same receiver required another maintained change", unchanged)
	}
	backlog := decode[m.RetargetReceipt](t, f.call("POST", path, f.token, "worker_one", "backlog-route", retargetInput(2, 0)), 200)
	if backlog.Revision != 3 || backlog.TargetPullNumber != 0 || !reflect.DeepEqual(backlog.AffectedPullNumbers, []int64{2898}) {
		t.Fatal("unassigned route lost source scope", backlog)
	}
	decode[m.RetargetReceipt](t, f.call("POST", path, f.token, "worker_one", "new-route", retargetInput(3, 3399)), 200)
	four := int64(4)
	resolution := m.ResolveRequest{ExpectedRevision: &four, Reason: "Verified the carried fix", Evidence: []string{"commit:verified-fix"}}
	decode[map[string]any](t, f.call("POST", base+"/"+id+"/resolve", f.token, "worker_one", "resolve-route", resolution), 200)
	detail = retargetDetail(t, f, id)
	if detail.Revision != 5 || detail.Status != "resolved" || detail.Resolution == nil || detail.Resolution.Revision != 5 || detail.Retarget == nil || detail.Retarget.Revision != 4 {
		t.Fatal("retarget and resolution did not share one revision", detail)
	}
	if replay := f.call("POST", path, f.token, "worker_one", "move-route", retargetInput(1, 3341)); replay.Code != 200 || replay.Body.String() != first.Body.String() {
		t.Fatal("lost-output replay changed historical receipt", replay.Body.String())
	}
	decode[map[string]any](t, f.call("POST", path, f.token, "worker_one", "closed-route", retargetInput(5, 3341)), 409)
	if retainedRetargetSource(t, f, id) != source || f.count("review_adoption_obligations") != 1 || f.count("review_adoption_retargets") != 3 {
		t.Fatal("retarget rewrote or duplicated imported work")
	}
	f.validate()
}

func TestRetargetReceiverMarkersPreserveUnprovenOriginalScopes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		targets  []int64
		evidence []string
		selected int64
		want     []int64
	}{
		{"same-marker-missing-current-guard", []int64{2898}, []string{"target-pr:3341"}, 3341, []int64{2898, 3341}},
		{"unassigned", []int64{2898}, []string{"target-pr:unassigned"}, 3341, []int64{2898, 3341}},
		{"unknown-to-explicit-backlog", []int64{2898}, []string{}, 0, []int64{2898}},
		{"conflicting", []int64{2898, 3105}, []string{"target-pr:3105", "target-pr:3341"}, 3399, []int64{2898, 3105, 3399}},
		{"receiver-only-to-empty-backlog", []int64{3105}, []string{"target-pr:3105"}, 0, []int64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fresh(t)
			id := retargetRoute(t, f, tc.targets, tc.evidence)
			receipt := decode[m.RetargetReceipt](t, f.call("POST", base+"/"+id+"/retarget", f.token, "worker_one", "select-receiver", retargetInput(1, tc.selected)), 200)
			if receipt.Revision != 2 || !reflect.DeepEqual(receipt.AffectedPullNumbers, tc.want) || !reflect.DeepEqual(retargetDetail(t, f, id).AffectedPullNumbers, tc.want) {
				t.Fatal("original source scope changed or explicit receiver ignored", receipt)
			}
			f.validate()
		})
	}
}

func TestRetargetCanAssignAnAdditionalReceiverToMaximumOriginalSourceScope(t *testing.T) {
	f := fresh(t)
	numbers := make([]int64, 100)
	for i := range numbers {
		numbers[i] = int64(i + 1)
	}
	id := retargetRoute(t, f, numbers, []string{"target-pr:unassigned"})
	receipt := decode[m.RetargetReceipt](t, f.call("POST", base+"/"+id+"/retarget", f.token, "worker_one", "max-source-scope", retargetInput(1, 3341)), 200)
	if len(receipt.AffectedPullNumbers) != 101 || receipt.AffectedPullNumbers[100] != 3341 {
		t.Fatal("valid original source scope could not acquire one receiver", receipt)
	}
	if detail := retargetDetail(t, f, id); len(detail.AffectedPullNumbers) != 101 || !reflect.DeepEqual(detail.Original.AffectedPullNumbers, numbers) {
		t.Fatal("maximum source scope changed", detail)
	}
	f.validate()
}

func retargetWriter(t *testing.T, f *fixture, name, project string, write bool) string {
	t.Helper()
	token := name + "-token"
	hash := sha256.Sum256([]byte(token))
	f.exec(`INSERT INTO credentials VALUES(?,?,?,0,0)`, name, name+"-principal", hex.EncodeToString(hash[:]))
	f.exec(`INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES(?,?,1,?)`, name, project, write)
	return token
}

func TestRetargetWriterAuthorityCASAndReplayAreScoped(t *testing.T) {
	f := fresh(t)
	id := retargetRoute(t, f, []int64{2898, 3105}, []string{"target-pr:3105"})
	writer := retargetWriter(t, f, "route-writer", "project_one", true)
	reader := retargetWriter(t, f, "route-reader", "project_one", false)
	foreign := retargetWriter(t, f, "route-foreign", "project_two", true)
	path := base + "/" + id + "/retarget"
	input := retargetInput(1, 3341)
	decode[map[string]any](t, f.call("POST", path, reader, "worker_one", "reader", input), 403)
	decode[map[string]any](t, f.call("POST", path, writer, "", "missing-worker", input), 403)
	decode[map[string]any](t, f.call("POST", path, writer, "worker_two", "foreign-worker", input), 404)
	decode[map[string]any](t, f.call("POST", path, foreign, "worker_one", "foreign-project", input), 403)
	decode[map[string]any](t, f.call("POST", strings.Replace(path, "project_one", "project_two", 1), foreign, "worker_two", "foreign-id", input), 404)
	first := f.call("POST", path, writer, "worker_one", "writer-retarget", input)
	decode[m.RetargetReceipt](t, first, 200)
	decode[map[string]any](t, f.call("POST", path, writer, "worker_one", "stale", retargetInput(1, 3399)), 409)
	decode[map[string]any](t, f.call("POST", path, writer, "worker_one", "writer-retarget", retargetInput(1, 3399)), 409)
	if detail := retargetDetail(t, f, id); detail.Retarget == nil || detail.Retarget.ActorID != "route-writer-principal" {
		t.Fatal("write switched acting identity", detail)
	}
	if replay := f.call("POST", path, writer, "worker_one", "writer-retarget", input); replay.Code != 200 || replay.Body.String() != first.Body.String() {
		t.Fatal("writer replay lost original receipt", replay.Body.String())
	}
	f.exec(`UPDATE grants SET can_write=0 WHERE credential_id='route-writer'`)
	decode[map[string]any](t, f.call("POST", path, writer, "worker_one", "writer-retarget", input), 403)
	f.exec(`UPDATE credentials SET revoked=1 WHERE id='route-writer'`)
	decode[map[string]any](t, f.call("POST", path, writer, "worker_one", "writer-retarget", input), 401)
	f.validate()
}

func TestRetargetConcurrentCASAndEventFailureAreAtomic(t *testing.T) {
	f := fresh(t)
	id := retargetRoute(t, f, []int64{2898, 3105}, []string{"target-pr:3105"})
	path := base + "/" + id + "/retarget"
	f.exec(`CREATE TRIGGER retarget_event_failure BEFORE INSERT ON events WHEN NEW.event_type='reviews.adoption_obligation_retargeted' BEGIN SELECT RAISE(ABORT,'controlled event failure'); END`)
	decode[map[string]any](t, f.call("POST", path, f.token, "worker_one", "failed-retarget", retargetInput(1, 3341)), 500)
	if d := retargetDetail(t, f, id); d.Revision != 1 || d.Retarget != nil || !reflect.DeepEqual(d.AffectedPullNumbers, []int64{2898, 3105}) || f.count("review_adoption_retargets") != 0 {
		t.Fatal("failed event left partial retarget", d)
	}
	f.exec(`DROP TRIGGER retarget_event_failure`)
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for i, target := range []int64{3341, 3399} {
		wg.Add(1)
		go func(i int, target int64) {
			defer wg.Done()
			statuses <- f.call("POST", path, f.token, "worker_one", fmt.Sprintf("race-%d", i), retargetInput(1, target)).Code
		}(i, target)
	}
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[200] != 1 || counts[409] != 1 || f.count("review_adoption_retargets") != 1 {
		t.Fatal("two retargets passed one revision", counts)
	}
	f.validate()
}

func TestRetargetRequiresOpenRouteAndExplicitBoundedReceiver(t *testing.T) {
	f := fresh(t)
	f.apply(batch(), "seed-kinds")
	ids := map[string]string{}
	for _, item := range f.page("?status=all").Items {
		ids[item.Kind] = item.ID
	}
	decode[map[string]any](t, f.call("POST", base+"/"+ids["finding"]+"/retarget", f.token, "worker_one", "finding-refusal", retargetInput(1, 3341)), 409)
	for i, body := range []string{
		`{"expected_revision":1,"reason":"current receiver"}`,
		`{"expected_revision":1,"target_pull_number":null,"reason":"current receiver"}`,
		`{"expected_revision":1,"target_pull_number":-1,"reason":"current receiver"}`,
		`{"expected_revision":1,"target_pull_number":2147483648,"reason":"current receiver"}`,
		`{"expected_revision":1,"target_pull_number":3341,"reason":""}`,
		`{"expected_revision":1,"target_pull_number":3341,"reason":"current receiver","affected_pull_numbers":[1]}`,
	} {
		decode[map[string]any](t, f.call("POST", base+"/"+ids["route"]+"/retarget", f.token, "worker_one", fmt.Sprintf("invalid-%d", i), []byte(body)), 400)
	}
	f.validate()
}

func TestRetargetSnapshotChecksHistoricalMetadataAndCurrentIndex(t *testing.T) {
	for _, corruption := range []string{"current-scope", "historical-scope", "historical-reason", "historical-time"} {
		t.Run(corruption, func(t *testing.T) {
			f := fresh(t)
			id := retargetRoute(t, f, []int64{2898, 3105}, []string{"target-pr:3105"})
			for i, target := range []int64{3341, 3399} {
				decode[m.RetargetReceipt](t, f.call("POST", base+"/"+id+"/retarget", f.token, "worker_one", fmt.Sprintf("retarget-%d", i), retargetInput(int64(i+1), target)), 200)
			}
			f.validate()
			if corruption == "current-scope" {
				f.exec(`INSERT INTO review_adoption_targets VALUES(?,'project_one',123)`, id)
			} else {
				f.exec(`DROP TRIGGER review_adoption_retargets_immutable`)
				switch corruption {
				case "historical-scope":
					f.exec(`UPDATE review_adoption_retargets SET targets_json='[]' WHERE obligation_id=? AND revision=2`, id)
				case "historical-reason":
					f.exec(`UPDATE review_adoption_retargets SET reason='' WHERE obligation_id=? AND revision=2`, id)
				case "historical-time":
					f.exec(`UPDATE review_adoption_retargets SET changed_at='bad' WHERE obligation_id=? AND revision=2`, id)
				}
				for _, statement := range h.SchemaSQL() {
					if strings.HasPrefix(statement, "CREATE TRIGGER review_adoption_retargets_immutable ") {
						f.exec(statement)
					}
				}
			}
			if err := h.ValidateSnapshot(t.Context(), f.store.DB()); err == nil {
				t.Fatal("corrupt current or historical retarget accepted")
			}
		})
	}
}

func TestPopulatedOriginalSchemaMigratesWithoutRewritingEvidence(t *testing.T) {
	f := &fixture{t: t, dir: filepath.Join(t.TempDir(), "legacy-adoption")}
	var err error
	f.store, err = core.Open(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	if err := f.store.EnsureSchema(t.Context(), "review_adoption_obligations", h.SchemaV1SQL()); err != nil {
		t.Fatal(err)
	}
	credential, err := os.ReadFile(filepath.Join(f.dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	f.token = strings.TrimSpace(string(credential))
	f.exec(`INSERT INTO projects VALUES('project_one','One',17,'owner/one')`)
	f.exec(`INSERT INTO workers VALUES('project_one','worker_one','One')`)
	f.service, f.mux = h.New(f.store), http.NewServeMux()
	f.service.Register(f.mux)
	f.apply(batch(), "old-originals")
	ids := map[string]string{}
	for _, item := range f.page("?status=all").Items {
		ids[item.Kind] = item.ID
	}
	one := int64(1)
	decode[map[string]any](t, f.call("POST", base+"/"+ids["finding"]+"/resolve", f.token, "worker_one", "old-resolution", m.ResolveRequest{ExpectedRevision: &one, Reason: "Resolved original finding", Evidence: []string{"commit:old-proof"}}), 200)
	prior := retainedRetargetSource(t, f, ids["route"])
	resolved := retargetDetail(t, f, ids["finding"])
	f.validate()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	f.validate()
	if after := retargetDetail(t, f, ids["finding"]); !reflect.DeepEqual(after, resolved) || prior != retainedRetargetSource(t, f, ids["route"]) {
		t.Fatal("upgrade rewrote original evidence or resolution", after, resolved)
	}
	decode[m.RetargetReceipt](t, f.call("POST", base+"/"+ids["route"]+"/retarget", f.token, "worker_one", "after-upgrade", retargetInput(1, 3399)), 200)
	f.validate()
	if err := h.EnsureSchema(context.Background(), f.store); err != nil {
		t.Fatal("repeat upgrade changed current state", err)
	}
}
