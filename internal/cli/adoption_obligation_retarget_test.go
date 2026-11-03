package cli_test

import (
	"encoding/json"
	"fmt"
	"github.com/benhook1013/FireController/internal/adoptionholds"
	"github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/client"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
)

func TestAdoptionObligationRetargetSyntaxBeforeProfiles(t *testing.T) {
	base := []string{"--profiles-file", "/missing/profile", "reviews", "obligations", "retarget", "adoption_obligation_00000000000000000000000000000000", "--expected-revision", "1", "--reason", "New receiver", "--key", "retained-key"}
	for _, flags := range [][]string{{}, {"--pr", "0"}, {"--pr", "42", "--pr", "43"}, {"--pr", "42", "--unassigned"}, {"--unassigned=true"}, {"--unassigned", "--unassigned"}, {"--pr", "42", "--unknown", "x"}} {
		code, out, diagnostic := run(append(append([]string{}, base...), flags...), nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(diagnostic, "invalid_arguments") {
			t.Fatal("invalid retarget reached profile", flags, code, diagnostic)
		}
	}
	for _, flags := range [][]string{{"--pr", "42"}, {"--unassigned"}} {
		code, _, diagnostic := run(append(append([]string{}, base...), flags...), nil)
		if code != cli.ExitConfig || !strings.Contains(diagnostic, "profile") {
			t.Fatal("valid retarget rejected before profile", flags, code, diagnostic)
		}
	}
}

func TestAdoptionObligationRetargetCLIActualReadReplayAndResolution(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "carried-route")
	worker := f.worker(t, project, "Builder")
	if err := adoptionholds.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	adoptionholds.New(f.store).Register(mux)
	f.handler = mux
	var repo int64
	if err := f.store.DB().QueryRow(`SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repo); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	original := adoptionholdsmodel.Obligation{SourceKey: "route-one", Kind: "route", AffectedPullNumbers: []int64{2, 7}, Summary: "Receiving correction", Evidence: []string{"target-pr:7"}, OriginalStatus: "open"}
	batch := adoptionholdsmodel.Batch{Version: 1, Source: adoptionholdsmodel.Source{Namespace: "retained", RepositoryID: repo, SnapshotSHA256: strings.Repeat("b", 64)}, ExpectedRevision: &zero, Obligations: []adoptionholdsmodel.Obligation{original}}
	base := "/api/v1/projects/" + project + "/reviews/adoption-obligations"
	preview := f.call(t, "POST", base+"/preview", f.admin, worker, batch)
	f.call(t, "POST", base+"/apply", f.admin, worker, map[string]any{"batch": batch, "preview_sha256": preview["preview_sha256"]})
	page := f.call(t, "GET", base, f.admin, worker, nil)
	id := page["items"].([]any)[0].(map[string]any)["id"].(string)
	service := httptest.NewServer(f.handler)
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	hits := f.resolverHit.Load()
	args := []string{"reviews", "obligations", "retarget", id, "--expected-revision", "1", "--pr", "9", "--reason", "Current receiving work", "--key", "retained-retarget"}
	code, out, diagnostic := mutationInvoke(t, path, "builder", args...)
	var receipt client.AdoptionObligationRetargetReceipt
	if code != 0 || diagnostic != "" || json.Unmarshal([]byte(out), &receipt) != nil || receipt.TargetPullNumber != 9 || receipt.Revision != 2 || receipt.Status != "open" {
		t.Fatal(code, out, diagnostic)
	}
	code, replay, diagnostic := mutationInvoke(t, path, "builder", args...)
	if code != 0 || diagnostic != "" || out != replay {
		t.Fatal("original retarget key failed", code, replay, diagnostic)
	}
	read := func() client.AdoptionObligation {
		t.Helper()
		code, body, stderr := mutationInvoke(t, path, "builder", "reviews", "obligations", "read", id)
		var detail client.AdoptionObligation
		if code != 0 || stderr != "" || json.Unmarshal([]byte(body), &detail) != nil {
			t.Fatal(code, body, stderr)
		}
		return detail
	}
	detail := read()
	if !reflect.DeepEqual(detail.Original, original) || !reflect.DeepEqual(detail.AffectedPullNumbers, []int64{2, 9}) || detail.Retarget == nil || detail.Retarget.TargetPullNumber != 9 {
		t.Fatal("origin/current receivers conflated", detail)
	}
	code, out, diagnostic = mutationInvoke(t, path, "builder", "reviews", "obligations", "retarget", id, "--expected-revision", "2", "--unassigned", "--reason", "Receiver needs assignment", "--key", "unassign-route")
	if code != 0 || diagnostic != "" || json.Unmarshal([]byte(out), &receipt) != nil || receipt.Revision != 3 || receipt.TargetPullNumber != 0 {
		t.Fatal(code, out, diagnostic)
	}
	detail = read()
	if detail.Status != "open" || !reflect.DeepEqual(detail.AffectedPullNumbers, []int64{2}) {
		t.Fatal("unassigned work resolved or source scope lost", detail)
	}
	code, out, diagnostic = mutationInvoke(t, path, "builder", "reviews", "obligations", "resolve", id, "--expected-revision", fmt.Sprint(detail.Revision), "--reason", "Correction verified", "--evidence", "proof:published-fix", "--key", "resolve-real-proof")
	if code != 0 || diagnostic != "" {
		t.Fatal(code, out, diagnostic)
	}
	detail = read()
	if detail.Status != "resolved" || detail.Revision != 4 || detail.Resolution == nil || detail.Resolution.Revision != 4 || !reflect.DeepEqual(detail.Original, original) {
		t.Fatal("resolve did not use current revision", detail)
	}
	if f.resolverHit.Load() != hits {
		t.Fatal("retarget or local reads called provider")
	}
}
