package cli_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/publicsite"
)

type preparedPublicationReceipt struct {
	ExpectedRevision int64                     `json:"expected_revision"`
	SHA256           string                    `json:"preview_sha256"`
	PreparedFile     string                    `json:"prepared_file"`
	Changes          client.PublicationChanges `json:"changes"`
}

func TestPublicationPrepareActualPinnedSelectionsPreserveEditorialAndApplyExactly(t *testing.T) {
	f := mapCLIService(t)
	if err := publicsite.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	publicsite.New(f.store, nil).Register(mux, t.Context())
	f.handler = mux
	project := f.project(t, "prepare-publication")
	worker := f.worker(t, project, "PRIVATE_WORKER")
	base := "/api/v1/projects/" + project
	job := f.call(t, "POST", base+"/jobs", f.admin, worker, map[string]any{"title": "PRIVATE_TITLE", "brief": "PRIVATE_BRIEF", "worker_id": worker})["id"].(string)
	f.call(t, "PUT", base+"/jobs/"+job+"/summary", f.admin, worker, map[string]any{"expected_job_revision": 1, "expected_revision": 0, "title": "Approved job", "body": "Approved summary"})
	entry := f.call(t, "POST", base+"/map/entries", f.admin, worker, map[string]any{"kind": "workstream", "title": "Approved direction", "description": "Approved map", "where": "PRIVATE_WHERE", "next": "PRIVATE_NEXT"})["id"].(string)
	var reads, previews atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/publication") {
			reads.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/publication/preview") {
			previews.Add(1)
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	profiles := mutationProfiles(t, f, srv.URL, map[string][2]string{"chosen": {project, worker}})
	invoke := func(args ...string) string {
		t.Helper()
		code, out, diagnostic := mutationInvoke(t, profiles, "chosen", args...)
		if code != 0 || diagnostic != "" {
			t.Fatal(code, out, diagnostic)
		}
		return out
	}
	seed := publicationCLIFile(t, `{"expected_revision":0,"enabled":false,"manifest":{"slug":"prepared-public","presentation":{"name":"Approved project"},"description":"Keep description","links":[{"key":"docs","label":"Guide","url":"https://example.com/guide"}],"project_map":[{"key":"authored_map","title":"Keep authored map","description":"Keep markdown"}],"progress":[{"key":"authored_progress","title":"Keep authored progress"}],"worker_lanes":[{"key":"authored_lane","label":"Keep lane","headline":"Keep headline"}],"reviews":[{"number":2,"title":"Keep review","url":"https://github.com/example/project/pull/2"}]}}`)
	var seedPreview client.PublicationPreview
	if err := json.Unmarshal([]byte(invoke("publication", "preview", "--file", seed)), &seedPreview); err != nil {
		t.Fatal(err)
	}
	invoke("publication", "apply", "--file", seed, "--preview-sha256", seedPreview.SHA256, "--key", "seed-publication")
	plan := map[string]any{"expected_revision": 1, "job_refs": map[string]any{"upsert": []any{map[string]any{"key": "job", "job_id": job, "summary_revision": 1}}}, "map_refs": map[string]any{"upsert": []any{map[string]any{"key": "direction", "entry_id": entry, "revision": 1}}}, "worker_refs": map[string]any{"upsert": []any{map[string]any{"key": "builder", "worker_id": worker, "label": "Approved builder"}}}, "reviews": map[string]any{"upsert": []any{map[string]any{"number": 3, "title": "Selected review", "url": "https://github.com/example/project/pull/3"}}}}
	raw, _ := json.Marshal(plan)
	changes := publicationCLIFile(t, string(raw))
	output := filepath.Join(t.TempDir(), "prepared.json")
	before := mapLedgerCounts(t, f)
	beforeReads, beforePreviews := reads.Load(), previews.Load()
	beforeProviders := f.resolverHit.Load()
	response := invoke("publication", "prepare", "--file", changes, "--output", output)
	var receipt preparedPublicationReceipt
	if err := json.Unmarshal([]byte(response), &receipt); err != nil || receipt.ExpectedRevision != 1 || receipt.PreparedFile != output || len(receipt.SHA256) != 64 || receipt.Changes.JobRefs.Upsert[0].SummaryRevision != 1 {
		t.Fatal(response, err)
	}
	if len(response) > 16<<10 || strings.Contains(response, "PRIVATE") || strings.Contains(response, "Keep markdown") || reads.Load() != beforeReads+1 || previews.Load() != beforePreviews+1 || !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
		t.Fatal("prepare echoed full prose, reread, or mutated", response)
	}
	if f.resolverHit.Load() != beforeProviders {
		t.Fatal("preparation contacted a provider")
	}
	info, err := os.Lstat(output)
	if err != nil || !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("prepared file not private regular", info, err)
	}
	preparedRaw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := client.DecodePublicationInput(preparedRaw)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Enabled || prepared.Manifest.Slug != seedPreview.Manifest.Slug || prepared.Manifest.Description != seedPreview.Manifest.Description || !reflect.DeepEqual(prepared.Manifest.ProjectMap, seedPreview.Manifest.ProjectMap) || !reflect.DeepEqual(prepared.Manifest.Progress, seedPreview.Manifest.Progress) || !reflect.DeepEqual(prepared.Manifest.WorkerLanes, seedPreview.Manifest.WorkerLanes) || !reflect.DeepEqual(prepared.Manifest.Links, seedPreview.Manifest.Links) || len(prepared.Manifest.Reviews) != 2 || prepared.Manifest.Reviews[0].Number != 2 || prepared.Manifest.Reviews[1].Number != 3 {
		t.Fatal("prepare altered unselected editorial content", prepared)
	}
	if strings.Contains(string(preparedRaw), "PRIVATE") || strings.Contains(string(preparedRaw), "Approved summary") {
		t.Fatal("private or fetched source prose in request", string(preparedRaw))
	}
	args := []string{"publication", "apply", "--file", output, "--preview-sha256", receipt.SHA256, "--key", "prepared-apply"}
	original := invoke(args...)
	before = mapLedgerCounts(t, f)
	if got := invoke(args...); got != original || !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
		t.Fatal("prepared apply recovery changed request", got)
	}
	// Replacing a selected ref keeps its position; removing another selection
	// leaves unmentioned records untouched. Explicit identical updates are safe.
	plan = map[string]any{"expected_revision": 2, "description": "", "reviews": map[string]any{"upsert": []any{map[string]any{"number": 3, "title": "Revised review", "url": "https://github.com/example/project/pull/3"}}, "remove": []int{2}}, "job_refs": map[string]any{"upsert": []any{map[string]any{"key": "job", "job_id": job, "summary_revision": 1}}}}
	raw, _ = json.Marshal(plan)
	changes = publicationCLIFile(t, string(raw))
	output = filepath.Join(t.TempDir(), "second.json")
	invoke("publication", "prepare", "--file", changes, "--output", output)
	preparedRaw, _ = os.ReadFile(output)
	prepared, err = client.DecodePublicationInput(preparedRaw)
	if err != nil || prepared.Manifest.Description != "" || len(prepared.Manifest.Reviews) != 1 || prepared.Manifest.Reviews[0].Title != "Revised review" || len(prepared.Manifest.JobRefs) != 1 {
		t.Fatal("explicit remove/upsert/clear lost", prepared, err)
	}
	other := f.project(t, "foreign-preparation")
	otherWorker := f.worker(t, other, "Other")
	foreignBase := "/api/v1/projects/" + other
	foreignJob := f.call(t, "POST", foreignBase+"/jobs", f.admin, otherWorker, map[string]any{"title": "FOREIGN_PRIVATE", "brief": "FOREIGN_PRIVATE"})["id"].(string)
	f.call(t, "PUT", foreignBase+"/jobs/"+foreignJob+"/summary", f.admin, otherWorker, map[string]any{"expected_job_revision": 1, "expected_revision": 0, "title": "FOREIGN_PUBLIC", "body": "Other project's approved summary"})
	for _, selection := range []map[string]any{
		{"key": "bad", "job_id": job, "summary_revision": 999},
		{"key": "bad", "job_id": foreignJob, "summary_revision": 1},
		{"key": "authored_map", "job_id": job, "summary_revision": 1},
	} {
		raw, _ := json.Marshal(map[string]any{"expected_revision": 2, "job_refs": map[string]any{"upsert": []any{selection}}})
		changes = publicationCLIFile(t, string(raw))
		output = filepath.Join(t.TempDir(), "refused.json")
		before = mapLedgerCounts(t, f)
		code, out, diagnostic := mutationInvoke(t, profiles, "chosen", "publication", "prepare", "--file", changes, "--output", output)
		if code == 0 || out != "" || strings.Contains(diagnostic, "FOREIGN") || !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
			t.Fatal("invalid selected source mutated or leaked", code, out, diagnostic)
		}
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			t.Fatal("invalid source wrote output", err)
		}
	}
}

func TestPublicationPrepareRacingOutputAndFailedReceiptKeepRecoverableFiles(t *testing.T) {
	f := publicationCLIService(t)
	project := f.project(t, "prepare-output")
	worker := f.worker(t, project, "Publisher")
	var collide atomic.Bool
	var collisionPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/publication/preview") && collide.Swap(false) {
			if err := os.WriteFile(collisionPath, []byte("concurrent file"), 0600); err != nil {
				t.Error(err)
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	profiles := mutationProfiles(t, f, srv.URL, map[string][2]string{"chosen": {project, worker}})
	seed := publicationCLIFile(t, publicationCLIInput)
	_, out, _ := mutationInvoke(t, profiles, "chosen", "publication", "preview", "--file", seed)
	var preview client.PublicationPreview
	if err := json.Unmarshal([]byte(out), &preview); err != nil {
		t.Fatal(err)
	}
	if code, _, diagnostic := mutationInvoke(t, profiles, "chosen", "publication", "apply", "--file", seed, "--preview-sha256", preview.SHA256, "--key", "seed"); code != 0 {
		t.Fatal(code, diagnostic)
	}
	changes := publicationCLIFile(t, `{"expected_revision":1,"description":"Approved change"}`)
	before := mapLedgerCounts(t, f)
	collisionPath = filepath.Join(t.TempDir(), "new.json")
	collide.Store(true)
	code, out, diagnostic := mutationInvoke(t, profiles, "chosen", "publication", "prepare", "--file", changes, "--output", collisionPath)
	if code == 0 || out != "" || !strings.Contains(diagnostic, "prepared_file_exists") || !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
		t.Fatal(code, out, diagnostic)
	}
	if raw, _ := os.ReadFile(collisionPath); string(raw) != "concurrent file" {
		t.Fatal("concurrent file replaced")
	}
	if files, _ := filepath.Glob(filepath.Join(filepath.Dir(collisionPath), ".fc-publication-*")); len(files) != 0 {
		t.Fatal("staging file leaked", files)
	}
	for _, writer := range []interface{ Write([]byte) (int, error) }{failedMutationOutput{}, shortMapOutput{}} {
		output := filepath.Join(t.TempDir(), "approved.json")
		var stderr bytes.Buffer
		code = cli.RunContext(t.Context(), []string{"--profiles-file", profiles, "--profile", "chosen", "publication", "prepare", "--file", changes, "--output", output}, nil, writer, &stderr)
		if code != cli.ExitConfig || !strings.Contains(stderr.String(), "prepared_receipt_output_failed") || strings.Contains(stderr.String(), "outcome_unknown") || !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
			t.Fatal("receipt loss hid recovery", code, stderr.String())
		}
		raw, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.DecodePublicationInput(raw); err != nil {
			t.Fatal("saved input lost", err)
		}
		if code, _, diagnostic := mutationInvoke(t, profiles, "chosen", "publication", "preview", "--file", output); code != 0 {
			t.Fatal("saved request cannot recover preview", code, diagnostic)
		}
	}
}

func TestPublicationPrepareInvalidFilesAndOutputBeforeCredentials(t *testing.T) {
	valid := publicationCLIFile(t, `{"expected_revision":1,"description":"Approved"}`)
	for _, args := range [][]string{
		{"--file", valid}, {"--file", valid, "--output", "relative"}, {"--file", valid, "--output", valid},
	} {
		code, out, diagnostic := run(append([]string{"--profiles-file", "/missing/PRIVATE-profile", "publication", "prepare"}, args...), nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(diagnostic, "invalid_arguments") {
			t.Fatal(code, out, diagnostic)
		}
	}
	for _, body := range []string{`{"expected_revision":1}`, `{"expected_revision":1,"enabled":true}`, `{"expected_revision":1,"job_refs":{"upsert":[{"key":"a","job_id":"job_` + strings.Repeat("a", 32) + `","summary_revision":0}]}}`} {
		file := publicationCLIFile(t, body)
		output := filepath.Join(t.TempDir(), "new.json")
		code, out, diagnostic := run([]string{"--profiles-file", "/missing/PRIVATE-profile", "publication", "prepare", "--file", file, "--output", output}, nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(diagnostic, "invalid_publication_changes") || strings.Contains(diagnostic, "PRIVATE") {
			t.Fatal(code, out, diagnostic)
		}
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			t.Fatal("invalid plan created output", err)
		}
	}
	existing := publicationCLIFile(t, "original")
	for _, output := range []string{existing, filepath.Join(t.TempDir(), "missing", "new.json")} {
		code, out, diagnostic := run([]string{"--profiles-file", "/missing/PRIVATE-profile", "publication", "prepare", "--file", valid, "--output", output}, nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(diagnostic, "prepared_file_") {
			t.Fatal(code, out, diagnostic)
		}
	}
	if raw, _ := os.ReadFile(existing); string(raw) != "original" {
		t.Fatal("existing file replaced")
	}
}

func TestPublicationPrepareRefusesMissingStaleAndMissingSelectionsWithoutMutation(t *testing.T) {
	f := publicationCLIService(t)
	project := f.project(t, "refuse-preparation")
	worker := f.worker(t, project, "Publisher")
	srv := httptest.NewServer(f.handler)
	defer srv.Close()
	profiles := mutationProfiles(t, f, srv.URL, map[string][2]string{"chosen": {project, worker}})
	try := func(body, expected string) {
		t.Helper()
		file := publicationCLIFile(t, body)
		output := filepath.Join(t.TempDir(), "new.json")
		before := mapLedgerCounts(t, f)
		code, out, diagnostic := mutationInvoke(t, profiles, "chosen", "publication", "prepare", "--file", file, "--output", output)
		if code == 0 || out != "" || !strings.Contains(diagnostic, expected) || !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
			t.Fatal(code, out, diagnostic)
		}
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			t.Fatal("refused preparation wrote output", err)
		}
	}
	try(`{"expected_revision":1,"description":"Approved"}`, "publication_not_configured")
	seed := publicationCLIFile(t, publicationCLIInput)
	_, out, _ := mutationInvoke(t, profiles, "chosen", "publication", "preview", "--file", seed)
	var preview client.PublicationPreview
	if err := json.Unmarshal([]byte(out), &preview); err != nil {
		t.Fatal(err)
	}
	if code, _, diagnostic := mutationInvoke(t, profiles, "chosen", "publication", "apply", "--file", seed, "--preview-sha256", preview.SHA256, "--key", "seed"); code != 0 {
		t.Fatal(code, diagnostic)
	}
	try(`{"expected_revision":2,"description":"Approved"}`, "publication_revision_conflict")
	try(`{"expected_revision":1,"job_refs":{"remove":["missing"]}}`, "invalid_publication_changes")
	try(`{"expected_revision":1,"job_refs":{"upsert":[{"key":"missing","job_id":"job_`+strings.Repeat("a", 32)+`","summary_revision":1}]}}`, "publication_source_unavailable")
}
