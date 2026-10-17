package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func mapImportCLIDocument() client.MapImportDocument {
	return client.MapImportDocument{Version: 1, Source: client.MapImportSource{Key: "prepared-map", RepositoryID: 1, Kind: "prepared", Snapshot: "selected document", CollectedAt: "2026-10-17T00:00:00Z", TransformationVersion: 1, Scope: "selected sources"}, Entries: []client.MapImportItem{{SourceKey: "track-one", Entry: client.ProjectMapEditable{Kind: "workstream", Title: "Initial track", Description: "PRIVATE_MAP_SOURCE", Phases: []client.ProjectMapPhase{}, Links: []client.ProjectMapLink{}}, Provenance: client.MapImportProvenance{References: []string{"https://example.com/source"}, Inferred: true, Confidence: "medium", Uncertainty: "Proposal"}}}}
}
func mapImportCLIFile(t *testing.T, raw []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "private-map-import.json")
	if e := os.WriteFile(p, raw, 0600); e != nil {
		t.Fatal(e)
	}
	return p
}
func mapImportCLIJSON(t *testing.T) []byte {
	t.Helper()
	b, e := json.Marshal(mapImportCLIDocument())
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func TestMapImportCLIInvalidArgumentsAndPrivateFileBeforeCredentials(t *testing.T) {
	file := mapImportCLIFile(t, mapImportCLIJSON(t))
	for _, args := range [][]string{{"import-preview"}, {"import-preview", "--file", "relative.json"}, {"import-preview", "--file", file, "--key", "key"}, {"import-preview", "--file", file, "--file", file}, {"import-apply", "--file", file, "--preview-sha256", strings.Repeat("B", 64), "--key", "key"}, {"import-apply", "--file", file, "--preview-sha256", strings.Repeat("b", 64)}, {"import-read", "bad/key"}, {"import-history", "prepared-map", "--limit", "101"}, {"import-history", "prepared-map", "--cursor", " bad"}, {"import-revision", "prepared-map", "01"}, {"import-revision", "prepared-map", "0"}} {
		code, out, stderr := run(append([]string{"--profiles-file", "/absent-private-profiles", "map"}, args...), nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatal(args, code, out, stderr)
		}
	}
	invalid := []string{filepath.Join(t.TempDir(), "missing"), t.TempDir()}
	for _, raw := range [][]byte{[]byte(`null`), []byte(`{"version":1,"version":1}`), bytes.Replace(mapImportCLIJSON(t), []byte(`"inferred":true`), []byte(`"inferred":null`), 1), bytes.Repeat([]byte("x"), (256<<10)+1)} {
		invalid = append(invalid, mapImportCLIFile(t, raw))
	}
	link := filepath.Join(t.TempDir(), "link")
	if e := os.Symlink(file, link); e == nil {
		invalid = append(invalid, link)
	}
	if runtime.GOOS != "windows" {
		exposed := mapImportCLIFile(t, mapImportCLIJSON(t))
		if e := os.Chmod(exposed, 0644); e != nil {
			t.Fatal(e)
		}
		invalid = append(invalid, exposed)
	}
	if program, e := exec.LookPath("mkfifo"); e == nil {
		fifo := filepath.Join(t.TempDir(), "pipe")
		if e = exec.Command(program, fifo).Run(); e != nil {
			t.Fatal(e)
		}
		invalid = append(invalid, fifo)
	}
	for _, p := range invalid {
		code, out, stderr := run([]string{"--profiles-file", "/absent-private-profiles", "map", "import-preview", "--file", p}, nil)
		if code != cli.ExitUsage || out != "" || strings.Contains(stderr, "profiles_") || strings.Contains(stderr, p) || strings.Contains(stderr, "PRIVATE_MAP_SOURCE") {
			t.Fatal("preflight leaked or loaded credentials", code, out, stderr)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out, stderr bytes.Buffer
	if code := cli.RunContext(ctx, []string{"map", "import-preview", "--file", file}, nil, &out, &stderr); code != cli.ExitCancelled || out.Len() != 0 || strings.Contains(stderr.String(), "outcome_unknown") {
		t.Fatal("pre-dispatch cancellation", code, stderr.String())
	}
}

func TestMapImportCLIActualPrivateProfilePreviewLostResponseAndOutputRecovery(t *testing.T) {
	f := mapCLIService(t)
	project := f.project(t, "map-import-cli")
	worker := f.worker(t, project, "Importer")
	var applies atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/map/import") {
			recorded := httptest.NewRecorder()
			f.handler.ServeHTTP(recorded, r)
			if recorded.Code == 200 && applies.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				io.WriteString(w, `{"source_key":`)
				return
			}
			for k, v := range recorded.Header() {
				for _, value := range v {
					w.Header().Add(k, value)
				}
			}
			w.WriteHeader(recorded.Code)
			w.Write(recorded.Body.Bytes())
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer s.Close()
	profiles := mutationProfiles(t, f, s.URL, map[string][2]string{"admin": {project, worker}})
	file := mapImportCLIFile(t, mapImportCLIJSON(t))
	provider := f.resolverHit.Load()
	var beforeReceipts, beforeEvents int
	f.store.DB().QueryRow(`SELECT count(*) FROM receipts`).Scan(&beforeReceipts)
	f.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&beforeEvents)
	code, out, stderr := mutationInvoke(t, profiles, "admin", "map", "import-preview", "--file", file)
	var preview client.MapImportPreview
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &preview) != nil || !preview.Ready || len(preview.Plan) != 1 {
		t.Fatal(code, out, stderr)
	}
	var receipts, events int
	f.store.DB().QueryRow(`SELECT count(*) FROM receipts`).Scan(&receipts)
	f.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&events)
	if receipts != beforeReceipts || events != beforeEvents {
		t.Fatal("preview changed durable state")
	}
	args := []string{"map", "import-apply", "--file", file, "--preview-sha256", preview.PreviewSHA256, "--key", "original-import-key"}
	code, out, stderr = mutationInvoke(t, profiles, "admin", args...)
	if code != cli.ExitService || out != "" || !strings.Contains(stderr, `"outcome_unknown":true`) || strings.Contains(stderr, "PRIVATE_MAP_SOURCE") || applies.Load() != 1 {
		t.Fatal("lost committed response", code, out, stderr)
	}
	code, out, stderr = mutationInvoke(t, profiles, "admin", args...)
	var first client.MapImportReceipt
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &first) != nil || first.Revision != 1 || first.Created != 1 {
		t.Fatal(first)
	}
	code, out, stderr = mutationInvoke(t, profiles, "admin", "map", "import-read", "prepared-map")
	var current client.MapImportRecord
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &current) != nil || current.Revision != 1 || len(current.Mappings) != 1 || current.WorkerID != worker {
		t.Fatal(code, out, stderr)
	}
	for _, read := range [][]string{{"map", "import-history", "prepared-map", "--limit", "1"}, {"map", "import-revision", "prepared-map", "1"}} {
		code, _, stderr = mutationInvoke(t, profiles, "admin", read...)
		if code != 0 || stderr != "" {
			t.Fatal(read, code, stderr)
		}
	}
	// Lose stdout after exact replay, then recover the same immutable receipt.
	var diagnostic bytes.Buffer
	command := append([]string{"--profiles-file", profiles, "--profile", "admin"}, args...)
	code = cli.Run(command, nil, mapImportFailWriter{}, &diagnostic)
	if code != cli.ExitConfig || !strings.Contains(diagnostic.String(), `"outcome_unknown":true`) || strings.Contains(diagnostic.String(), "PRIVATE_MAP_SOURCE") {
		t.Fatal("lost output", code, diagnostic.String())
	}
	code, out, stderr = mutationInvoke(t, profiles, "admin", args...)
	var again client.MapImportReceipt
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &again) != nil || again != first || f.resolverHit.Load() != provider {
		t.Fatal("recovery changed revision/contacted provider", again)
	}
	var count int
	f.store.DB().QueryRow(`SELECT count(*) FROM project_map_entries`).Scan(&count)
	if count != 1 {
		t.Fatal("recovery duplicated entries", count)
	}
}

type mapImportFailWriter struct{}

func (mapImportFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
