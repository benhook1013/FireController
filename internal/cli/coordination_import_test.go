package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
	"github.com/benhook1013/FireController/internal/coordimport"
	"github.com/benhook1013/FireController/internal/coordimportmodel"
)

func coordinationCLIBatch(worker string) coordimportmodel.Batch {
	zero := int64(0)
	return coordimportmodel.Batch{Version: 1, RepositoryID: 1, ExpectedRevision: &zero, Source: "selected historical archive", SourceSHA256: strings.Repeat("a", 64), Jobs: []coordimportmodel.PreparedJob{{SourceKey: "original-job", SourceRevision: 42,
		Title: "Historical job", Brief: "PRIVATE_COORDINATION_IMPORT_BODY", Status: "parked", WorkerID: worker, CreatedAt: "2024-01-02T03:04:05+13:00", UpdatedAt: "2024-02-03T04:05:06Z",
		Checkpoints: []coordimportmodel.PreparedCheckpoint{{SourceKey: "checkpoint", ActorID: "original-actor", WorkerID: worker, Done: "retained", Next: "continue", CreatedAt: "2024-02-03T04:05:06Z"}},
	}}}
}

func TestCoordinationImportCLIRealPreviewLostResultAndExactRecovery(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "coordination-import")
	worker := f.worker(t, project, "Importer")
	if err := coordimport.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	coordimport.Register(mux, f.store)
	var requests, applies atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if strings.HasSuffix(r.URL.Path, "/preview") && r.Header.Get("Idempotency-Key") != "" {
			t.Error("preview has mutation key")
		}
		if strings.HasSuffix(r.URL.Path, "/apply") {
			recorded := httptest.NewRecorder()
			mux.ServeHTTP(recorded, r)
			if recorded.Code == 200 && applies.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				_, _ = io.WriteString(w, `{"revision":`)
				return
			}
			for key, values := range recorded.Header() {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			w.WriteHeader(recorded.Code)
			_, _ = w.Write(recorded.Body.Bytes())
			return
		}
		mux.ServeHTTP(w, r)
	}))
	defer s.Close()
	profiles := mutationProfiles(t, f, s.URL, map[string][2]string{"admin": {project, worker}})
	batch := coordinationCLIBatch(worker)
	raw, _ := json.Marshal(batch)
	file := importCLIFile(t, raw)
	provider := f.resolverHit.Load()
	count := func() string {
		var n string
		if err := f.store.DB().QueryRow(`SELECT (SELECT count(*) FROM jobs)||':'||(SELECT count(*) FROM job_history)||':'||(SELECT count(*) FROM job_checkpoints)||':'||(SELECT count(*) FROM coordination_import_records)||':'||(SELECT count(*) FROM coordination_import_batches)||':'||(SELECT count(*) FROM events)||':'||(SELECT count(*) FROM receipts)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	code, out, stderr := mutationInvoke(t, profiles, "admin", "import", "state")
	if code != 0 || out != "{\"revision\":0}\n" || stderr != "" || requests.Load() != 2 || count() != before {
		t.Fatal("state is not a scoped quiet read", code, out, stderr)
	}
	code, out, stderr = mutationInvoke(t, profiles, "admin", "import", "preview", "--file", file)
	var preview client.CoordinationImportPreview
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &preview) != nil || preview.ProjectID != project || preview.Counts != (client.CoordinationImportCounts{Jobs: 1, Checkpoints: 1, NewJobs: 1}) || strings.Contains(out, batch.Jobs[0].Brief) || requests.Load() != 4 || count() != before {
		t.Fatal("preview is not compact/select-only", code, out, stderr)
	}
	args := []string{"import", "apply", "--file", file, "--preview", preview.PreviewSHA256, "--key", "exact-import"}
	code, out, stderr = mutationInvoke(t, profiles, "admin", args...)
	if code != cli.ExitService || out != "" || !strings.Contains(stderr, `"outcome_unknown":true`) || strings.Contains(stderr, batch.Jobs[0].Brief) || applies.Load() != 1 {
		t.Fatal("lost response lost single-shot uncertainty", code, out, stderr)
	}
	code, out, stderr = mutationInvoke(t, profiles, "admin", args...)
	var receipt client.CoordinationImportReceipt
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &receipt) != nil || receipt.Revision != 1 || receipt.Outcome != "imported" || len(receipt.Mappings) != 1 || strings.Contains(out, batch.Jobs[0].Brief) {
		t.Fatal("exact response recovery failed", code, out, stderr)
	}
	id := receipt.Mappings[0].JobID
	detail := f.call(t, "GET", "/api/v1/projects/"+project+"/jobs/"+id, f.admin, "", nil)
	if detail["revision"] != float64(1) || detail["brief"] != batch.Jobs[0].Brief || detail["created_at"] != batch.Jobs[0].CreatedAt {
		t.Fatal("native detail changed historical state")
	}
	// Ordinary edits and a later import do not change the original receipt or
	// allow the original source snapshot to overwrite the newer native brief.
	mutationSuccess(t, profiles, "admin", "jobs", "edit", id, "--expect-revision", "1", "--brief", "new native brief", "--key", "ordinary-edit")
	later := coordinationCLIBatch(worker)
	one := int64(1)
	later.ExpectedRevision = &one
	later.Jobs[0].SourceKey = "later-job"
	laterRaw, _ := json.Marshal(later)
	laterFile := importCLIFile(t, laterRaw)
	code, out, stderr = mutationInvoke(t, profiles, "admin", "import", "preview", "--file", laterFile)
	var laterPreview client.CoordinationImportPreview
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &laterPreview) != nil {
		t.Fatal("later preview", code, stderr)
	}
	code, _, stderr = mutationInvoke(t, profiles, "admin", "import", "apply", "--file", laterFile, "--preview", laterPreview.PreviewSHA256, "--key", "later-import")
	if code != 0 || stderr != "" {
		t.Fatal("later apply", code, stderr)
	}
	saved := count()
	code, out, stderr = mutationInvoke(t, profiles, "admin", args...)
	var replay client.CoordinationImportReceipt
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &replay) != nil || !reflect.DeepEqual(replay, receipt) || count() != saved {
		t.Fatal("historical exact receipt rejected/changed", code, stderr)
	}
	detail = f.call(t, "GET", "/api/v1/projects/"+project+"/jobs/"+id, f.admin, "", nil)
	if detail["revision"] != float64(2) || detail["brief"] != "new native brief" {
		t.Fatal("retry overwrote newer native edits")
	}
	var diagnostic bytes.Buffer
	fullArgs := append([]string{"--profiles-file", profiles, "--profile", "admin"}, args...)
	if code := cli.RunContext(t.Context(), fullArgs, nil, coordinationShortWriter{}, &diagnostic); code != cli.ExitConfig || !strings.Contains(diagnostic.String(), `"outcome_unknown":true`) || !strings.Contains(diagnostic.String(), "exact same file contents") || count() != saved {
		t.Fatal("short output did not retain exact retry guidance", code, diagnostic.String())
	}
	batch.Jobs[0].Brief = "changed selected history"
	changed, _ := json.Marshal(batch)
	if err := os.WriteFile(file, changed, 0600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr = mutationInvoke(t, profiles, "admin", args...)
	if code != cli.ExitConflict || out != "" || !strings.Contains(stderr, "import_preview_mismatch") || strings.Contains(stderr, `"outcome_unknown":true`) || count() != saved || f.resolverHit.Load() != provider {
		t.Fatal("changed content/provider isolation boundary failed", code, out, stderr)
	}
}

type coordinationShortWriter struct{}

func (coordinationShortWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }

func TestCoordinationImportCLIPreflightBeforeCredentials(t *testing.T) {
	missingProfile := filepath.Join(t.TempDir(), "not-a-profile.json")
	valid, _ := json.Marshal(coordinationCLIBatch(""))
	file := importCLIFile(t, valid)
	for _, args := range [][]string{
		{"import"}, {"import", "unknown"}, {"import", "state", "--file", file},
		{"import", "preview"}, {"import", "preview", "--file", "relative.json"},
		{"import", "preview", "--file", file, "--file", file}, {"import", "preview", "--file", file, "--unknown", "value"},
		{"import", "apply", "--file", file, "--preview", "bad", "--key", "key"},
		{"import", "apply", "--file", file, "--preview", strings.Repeat("a", 64)},
		{"import", "apply", "--file", file, "--preview", strings.Repeat("a", 64), "--key", "bad\nkey"},
	} {
		code, stdout, stderr := run(append([]string{"--profiles-file", missingProfile}, args...), nil)
		if code != cli.ExitUsage || stdout != "" || !strings.Contains(stderr, "invalid_arguments") || strings.Contains(stderr, missingProfile) {
			t.Fatal("invalid syntax reached profile", args, code, stdout, stderr)
		}
	}
	for name, data := range map[string][]byte{
		"bad-json": []byte("{"), "duplicate": []byte(`{"version":1,"version":1}`), "unknown": append([]byte(`{"unknown":true,`), valid[1:]...),
		"null": []byte("null"), "oversize": []byte(strings.Repeat("x", (256<<10)+1)),
	} {
		t.Run(name, func(t *testing.T) {
			path := importCLIFile(t, data)
			code, stdout, stderr := run([]string{"--profiles-file", missingProfile, "import", "preview", "--file", path}, nil)
			if code == cli.ExitSuccess || stdout != "" || strings.Contains(stderr, "profile_required") || strings.Contains(stderr, "profiles_") || strings.Contains(stderr, path) {
				t.Fatal("malformed file reached credentials or leaked path", code, stdout, stderr)
			}
		})
	}
	for name, prepare := range map[string]func(string){
		"symlink": func(path string) {
			if err := os.Symlink(file, path); err != nil {
				t.Skip("symlink unavailable")
			}
		},
		"directory": func(path string) {
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
		},
		"exposed": func(path string) {
			if runtime.GOOS == "windows" {
				t.Skip("POSIX owner mode")
			}
			if err := os.WriteFile(path, valid, 0644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "selected")
			prepare(path)
			code, out, stderr := run([]string{"--profiles-file", missingProfile, "import", "preview", "--file", path}, nil)
			if code == 0 || out != "" || !strings.Contains(stderr, "invalid_import_file") || strings.Contains(stderr, path) {
				t.Fatal("unsafe file passed preflight", code, out, stderr)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var stdout, stderr bytes.Buffer
	code := cli.RunContext(ctx, []string{"import", "preview", "--file", file}, nil, &stdout, &stderr)
	if code != cli.ExitCancelled || stdout.Len() != 0 || strings.Contains(stderr.String(), "profile_required") || strings.Contains(stderr.String(), `"outcome_unknown":true`) {
		t.Fatal("unsent cancellation reached configuration/action", code, stderr.String())
	}
}
