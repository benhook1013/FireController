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
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/publicsite"
)

const publicationCLIInput = `{"expected_revision":0,"enabled":true,"manifest":{"slug":"public-cli","presentation":{"name":"Approved CLI <literal>"},"description":"Approved project description"}}`

func publicationCLIService(t *testing.T) *serviceFixture {
	t.Helper()
	f := setupServiceFixture(t)
	if err := publicsite.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	publicsite.New(f.store, nil).Register(mux, t.Context())
	f.handler = mux
	return f
}

func publicationCLIFile(t *testing.T, body string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "approved-publication.json")
	if err := os.WriteFile(file, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestPublicationCLIActualProfilePreviewApplyRetryAndUnpublish(t *testing.T) {
	f := publicationCLIService(t)
	project, other := f.project(t, "publication-cli"), f.project(t, "other-publication")
	worker, otherWorker := f.worker(t, project, "Publisher"), f.worker(t, other, "Other")
	srv := httptest.NewServer(f.handler)
	defer srv.Close()
	profiles := mutationProfiles(t, f, srv.URL, map[string][2]string{"chosen": {project, worker}, "other": {other, otherWorker}})
	file := publicationCLIFile(t, publicationCLIInput)
	invoke := func(args ...string) string {
		t.Helper()
		code, out, err := mutationInvoke(t, profiles, "chosen", args...)
		if code != 0 || err != "" {
			t.Fatal(code, out, err)
		}
		return out
	}
	before := mapLedgerCounts(t, f)
	if raw := invoke("publication", "read"); !strings.Contains(raw, `"revision":0`) || !strings.Contains(raw, `"manifest":null`) {
		t.Fatal("initial read lost implicit state", raw)
	}
	var preview client.PublicationPreview
	if err := json.Unmarshal([]byte(invoke("publication", "preview", "--file", file)), &preview); err != nil || len(preview.SHA256) != 64 || preview.Content.Presentation.Name != "Approved CLI <literal>" || !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
		t.Fatal("preview failed or mutated", preview, err)
	}
	args := []string{"publication", "apply", "--file", file, "--preview-sha256", preview.SHA256, "--key", "approved-original"}
	original := invoke(args...)
	if !strings.Contains(original, `"revision":1`) || len(original) > 100 || strings.Contains(original, "Approved") {
		t.Fatal("write did not return compact receipt", original)
	}
	before = mapLedgerCounts(t, f)
	if got := invoke(args...); got != original || !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
		t.Fatal("exact apply retry changed effects", got)
	}
	if raw := invoke("publication", "read"); !strings.Contains(raw, `"enabled":true`) || !strings.Contains(raw, "Approved CLI") {
		t.Fatal("published settings unavailable", raw)
	}
	unpublish := []string{"publication", "unpublish", "--expect-revision", "1", "--key", "unpublish-original"}
	unpublished := invoke(unpublish...)
	if !strings.Contains(unpublished, `"revision":2`) {
		t.Fatal(unpublished)
	}
	before = mapLedgerCounts(t, f)
	if got := invoke(unpublish...); got != unpublished || !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
		t.Fatal("unpublish retry changed effects", got)
	}
	if got := invoke(args...); got != original || !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
		t.Fatal("historical apply replay republished or changed receipt", got)
	}
	if raw := invoke("publication", "read"); !strings.Contains(raw, `"enabled":false`) {
		t.Fatal("replay republished disabled project", raw)
	}
	code, out, err := mutationInvoke(t, profiles, "other", "publication", "read")
	if code != 0 || err != "" || strings.Contains(out, "Approved CLI") || !strings.Contains(out, `"revision":0`) {
		t.Fatal("profile isolation lost", code, out, err)
	}
	if f.resolverHit.Load() != 2 {
		t.Fatal("publication performed provider work")
	}
}

func TestPublicationCLIInvalidFilesAndArgumentsBeforeProfileLoad(t *testing.T) {
	good := publicationCLIFile(t, publicationCLIInput)
	for _, args := range [][]string{
		{"read", "extra"}, {"read", "--file", good}, {"preview"}, {"preview", "--file", "relative"}, {"preview", "--file", good, "--file", good},
		{"apply", "--file", good, "--key", "key"}, {"apply", "--file", good, "--preview-sha256", strings.Repeat("A", 64), "--key", "key"}, {"apply", "--file", good, "--preview-sha256", strings.Repeat("a", 64), "--key", " padded "},
		{"unpublish", "--expect-revision", "0"}, {"unpublish", "--expect-revision", "-1", "--key", "key"}, {"unpublish", "--expect-revision", "9223372036854775807", "--key", "key"}, {"unpublish", "--expect-revision", "0", "--key", "key", "--worker", "foreign"},
	} {
		code, out, err := run(append([]string{"--profiles-file", "/missing/PRIVATE-profile", "--profile", "absent", "publication"}, args...), nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_arguments") || strings.Contains(err, "PRIVATE") {
			t.Fatal(code, out, err)
		}
	}
	for _, body := range []string{
		`{}`, strings.Replace(publicationCLIInput, `"expected_revision":0`, `"expected_revision":null`, 1), strings.Replace(publicationCLIInput, `"enabled":true`, `"enabled":null`, 1), strings.Replace(publicationCLIInput, `"slug":"public-cli"`, `"slug":"PRIVATE","slug":"alias"`, 1), strings.Replace(publicationCLIInput, `"name":"Approved CLI <literal>"`, `"name":"PRIVATE","Name":"alias"`, 1), strings.Replace(publicationCLIInput, `"description":"Approved project description"`, `"description":"PRIVATE","private_actor":"sentinel"`, 1), string([]byte{0xff}), strings.Repeat("x", client.MaxPublicationDocumentBytes+1),
	} {
		file := publicationCLIFile(t, body)
		code, out, err := run([]string{"--profiles-file", "/missing/PRIVATE-profile", "--profile", "absent", "publication", "preview", "--file", file}, nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_publication_file") || strings.Contains(err, "PRIVATE") {
			t.Fatal("invalid file reached credentials or leaked content", code, out, err)
		}
	}
	paths := []string{t.TempDir()}
	link := filepath.Join(t.TempDir(), "symlink")
	if err := os.Symlink(good, link); err == nil {
		paths = append(paths, link)
	}
	if runtime.GOOS != "windows" {
		nonprivate := publicationCLIFile(t, publicationCLIInput)
		if err := os.Chmod(nonprivate, 0644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, nonprivate)
	}
	for _, file := range paths {
		code, out, err := run([]string{"--profiles-file", "/missing/PRIVATE-profile", "publication", "preview", "--file", file}, nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_publication_file") {
			t.Fatal("nonregular/nonprivate file reached profile loading", code, out, err)
		}
	}
}

func TestPublicationCLILostCommittedReceiptAndFailedOutputRecoverExactly(t *testing.T) {
	f := publicationCLIService(t)
	project := f.project(t, "publication-output")
	worker := f.worker(t, project, "Publisher")
	var lose atomic.Bool
	lose.Store(true)
	var applies atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/publication") {
			applies.Add(1)
			if lose.Swap(false) {
				record := httptest.NewRecorder()
				f.handler.ServeHTTP(record, r)
				if record.Code != 200 {
					t.Error("real apply did not commit", record.Code)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, "{")
				return
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	profiles := mutationProfiles(t, f, srv.URL, map[string][2]string{"chosen": {project, worker}})
	file := publicationCLIFile(t, publicationCLIInput)
	code, out, diagnostic := mutationInvoke(t, profiles, "chosen", "publication", "preview", "--file", file)
	if code != 0 || diagnostic != "" {
		t.Fatal(code, out, diagnostic)
	}
	var preview client.PublicationPreview
	if err := json.Unmarshal([]byte(out), &preview); err != nil {
		t.Fatal(err)
	}
	args := []string{"publication", "apply", "--file", file, "--preview-sha256", preview.SHA256, "--key", "original"}
	code, out, diagnostic = mutationInvoke(t, profiles, "chosen", args...)
	if code == 0 || out != "" || !strings.Contains(diagnostic, `"outcome_unknown":true`) || applies.Load() != 1 {
		t.Fatal("lost commit was hidden or retried", code, out, diagnostic)
	}
	before := mapLedgerCounts(t, f)
	for _, writer := range []io.Writer{failedMutationOutput{}, shortMapOutput{}} {
		var stderr bytes.Buffer
		code = cli.RunContext(t.Context(), append([]string{"--profiles-file", profiles, "--profile", "chosen"}, args...), nil, writer, &stderr)
		if code != cli.ExitConfig || !strings.Contains(stderr.String(), "output_failed") || !strings.Contains(stderr.String(), `"outcome_unknown":true`) || !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
			t.Fatal("write output failure lost recovery semantics", code, stderr.String())
		}
	}
	code, out, diagnostic = mutationInvoke(t, profiles, "chosen", args...)
	if code != 0 || diagnostic != "" || !strings.Contains(out, `"revision":1`) || !reflect.DeepEqual(before, mapLedgerCounts(t, f)) {
		t.Fatal("exact output recovery changed committed state", code, out, diagnostic)
	}
}

func TestPublicationCLIFIFORefusalExternallyBounded(t *testing.T) {
	if file := os.Getenv("FC_PUBLICATION_FIFO_CHILD"); file != "" {
		code, out, err := run([]string{"--profiles-file", "/missing/PRIVATE-profile", "publication", "preview", "--file", file}, nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_publication_file") {
			t.Fatal(code, out, err)
		}
		return
	}
	if runtime.GOOS == "windows" {
		t.Skip("Unix FIFO")
	}
	mkfifo, err := exec.LookPath("mkfifo")
	if err != nil {
		t.Skip("mkfifo unavailable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	file := filepath.Join(t.TempDir(), "publication.fifo")
	if out, err := exec.CommandContext(ctx, mkfifo, file).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(ctx, exe, "-test.run=^TestPublicationCLIFIFORefusalExternallyBounded$")
	child.Env = append(os.Environ(), "FC_PUBLICATION_FIFO_CHILD="+file)
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatal("FIFO blocked or failed", err, string(out))
	}
}
