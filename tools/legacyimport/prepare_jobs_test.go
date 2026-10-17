package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/coordimportmodel"
	"github.com/benhook1013/FireController/tools/legacyimport/internal/legacy"
)

const prepareFixtureBrief = "  PRIVATE_CONVERTER_BRIEF 雪\n\tunchanged  "

func prepareCLIFixture(t *testing.T, brief string) (string, string, legacy.JobSelection) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(dir, "PRIVATE_SELECTED_SNAPSHOT?#.sqlite")
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(snapshot)}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema, err := os.ReadFile(filepath.Join("internal", "legacy", "testdata", "snapshot-schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, values ...any) {
		t.Helper()
		if _, err := db.ExecContext(t.Context(), query, values...); err != nil {
			t.Fatal(err)
		}
	}
	exec("BEGIN;" + string(schema))
	for _, statement := range []string{
		`INSERT INTO controller_metadata VALUES(1,3,11)`, `INSERT INTO review_state VALUES(1,'{"schema_version":3}')`,
		`INSERT INTO job_metadata VALUES(1,2,1,3)`, `INSERT INTO inbox_metadata VALUES(1,1)`,
		`INSERT INTO map_metadata VALUES(1,1,1,3)`, `INSERT INTO review_records_metadata VALUES(1,9,1,3,11)`,
		`INSERT INTO jobs VALUES('PRIVATE_SOURCE_JOB','PRIVATE_JOB_NAME','PRIVATE_LEGACY_WORKER',NULL,'PRIVATE_JOB_TITLE','parked',0,9,3,'PRIVATE_SUMMARY','','','[]',NULL,'2024-01-02T03:04:05+13:00','2024-02-03T04:05:06Z')`,
	} {
		exec(statement)
	}
	exec(`INSERT INTO job_briefs VALUES('PRIVATE_SOURCE_JOB',3,'2024-02-03T04:05:06Z',?)`, brief)
	exec(`INSERT INTO job_checkpoints(job_id,created_at,done,next_steps,blocker,pointers_json) VALUES('PRIVATE_SOURCE_JOB','2024-02-03T04:05:06Z','PRIVATE_FIRST_DONE','next','','{}')`)
	exec(`INSERT INTO job_checkpoints(job_id,created_at,done,next_steps,blocker,pointers_json) VALUES('PRIVATE_SOURCE_JOB','2024-02-04T05:06:07+13:00','PRIVATE_LATEST_DONE','  next step  ','blocked','{"pr":7,"evidence":["first","second"]}')`)
	exec("COMMIT")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(snapshot, 0600); err != nil {
		t.Fatal(err)
	}
	revision := int64(0)
	selection := legacy.JobSelection{Version: 1, RepositoryID: 71, ExpectedRevision: &revision, Source: "selected-legacy-export", Workers: map[string]string{"PRIVATE_LEGACY_WORKER": "worker_native"}, JobIDs: []string{"PRIVATE_SOURCE_JOB"}, CheckpointMode: "latest"}
	selectionFile := filepath.Join(dir, "PRIVATE_SELECTION.json")
	writePrepareSelection(t, selectionFile, selection)
	return snapshot, selectionFile, selection
}

func writePrepareSelection(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func prepareCLIArgs(snapshot, selection, output string) []string {
	return []string{"prepare-jobs", "--snapshot", snapshot, "--selection", selection, "--output", output}
}

func assertPrepareSourceConserved(t *testing.T, path string, original []byte) {
	t.Helper()
	now, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(now, original) {
		t.Fatal("preparation changed source bytes", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !os.IsNotExist(err) {
			t.Fatal("preparation created source sidecar")
		}
	}
}

func assertPrepareSafe(t *testing.T, output []byte, privatePaths ...string) {
	t.Helper()
	for _, secret := range append(privatePaths, "PRIVATE_SOURCE_JOB", "PRIVATE_JOB_NAME", "PRIVATE_LEGACY_WORKER", "PRIVATE_JOB_TITLE", "PRIVATE_CONVERTER_BRIEF", "PRIVATE_FIRST_DONE", "PRIVATE_LATEST_DONE") {
		if bytes.Contains(output, []byte(secret)) {
			t.Fatal("ordinary output exposed private source data")
		}
	}
}

func TestPrepareJobsCLIPublishesPrivateSnapshotAndExactRepeat(t *testing.T) {
	snapshot, selection, _ := prepareCLIFixture(t, prepareFixtureBrief)
	before, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(filepath.Dir(snapshot), "prepared.json")
	args := prepareCLIArgs(snapshot, selection, output)
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), args, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatal("preparation failed", code, stderr.String())
	}
	assertPrepareSafe(t, stdout.Bytes(), snapshot, selection, output)
	var summary struct {
		Kind         string `json:"kind"`
		SourceSHA256 string `json:"source_sha256"`
		Jobs         int    `json:"jobs"`
		Checkpoints  int    `json:"checkpoints"`
	}
	digest := sha256.Sum256(before)
	if json.Unmarshal(stdout.Bytes(), &summary) != nil || summary.Kind != "prepared_job_import" || summary.Jobs != 1 || summary.Checkpoints != 1 || summary.SourceSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("compact summary did not describe selected scope")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(stdout.Bytes(), &fields) != nil || len(fields) != 4 {
		t.Fatal("summary expanded beyond compact counts and digest")
	}
	prepared, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var batch coordimportmodel.Batch
	if json.Unmarshal(prepared, &batch) != nil || batch.Version != 1 || batch.RepositoryID != 71 || batch.SourceSHA256 != summary.SourceSHA256 || len(batch.Jobs) != 1 {
		t.Fatal("invalid native prepared file")
	}
	item := batch.Jobs[0]
	if item.SourceRevision != 9 || item.SourceKey != "PRIVATE_SOURCE_JOB" || item.WorkerID != "worker_native" || item.Brief != prepareFixtureBrief || item.CreatedAt != "2024-01-02T03:04:05+13:00" || item.UpdatedAt != "2024-02-03T04:05:06Z" || len(item.Checkpoints) != 1 {
		t.Fatal("selected historical current snapshot changed")
	}
	cp := item.Checkpoints[0]
	if cp.SourceKey != "2" || cp.Done != "PRIVATE_LATEST_DONE" || cp.Next != "  next step  " || cp.ActorID != "imported-history" || cp.WorkerID != "" || cp.CreatedAt != "2024-02-04T05:06:07+13:00" || !reflect.DeepEqual(cp.Evidence, []string{`{"pr":7,"evidence":["first","second"]}`}) {
		t.Fatal("selected checkpoint facts changed or scope widened")
	}
	info, err := os.Lstat(output)
	if err != nil || !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("prepared output is not owner-private regular file", err)
	}
	stdout.Reset()
	if code := run(t.Context(), args, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatal("exact repeat failed", code)
	}
	repeated, err := os.ReadFile(output)
	newInfo, statErr := os.Lstat(output)
	if err != nil || statErr != nil || !bytes.Equal(prepared, repeated) || !os.SameFile(info, newInfo) {
		t.Fatal("identical repeat replaced/changed existing output")
	}
	assertPrepareSourceConserved(t, snapshot, before)
	if pending, err := filepath.Glob(filepath.Join(filepath.Dir(output), ".legacyimport-*")); err != nil || len(pending) != 0 {
		t.Fatal("temporary output files leaked")
	}
}

type prepareShortWriter struct{}

func (prepareShortWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }

func TestPrepareJobsCLIOutputRecoveryAndConflict(t *testing.T) {
	snapshot, selection, _ := prepareCLIFixture(t, prepareFixtureBrief)
	before, _ := os.ReadFile(snapshot)
	output := filepath.Join(filepath.Dir(snapshot), "prepared.json")
	args := prepareCLIArgs(snapshot, selection, output)
	var stderr bytes.Buffer
	if code := run(t.Context(), args, prepareShortWriter{}, &stderr); code != 3 || readDiagnostic(t, stderr.Bytes()) != "output_unavailable" {
		t.Fatal("short summary output not reported safely", code)
	}
	assertPrepareSafe(t, stderr.Bytes(), snapshot, selection, output)
	prepared, err := os.ReadFile(output)
	if err != nil {
		t.Fatal("committed prepared file unavailable after stdout failure", err)
	}
	var batch coordimportmodel.Batch
	if json.Unmarshal(prepared, &batch) != nil || len(batch.Jobs) != 1 {
		t.Fatal("stdout failure left invalid prepared file")
	}
	var stdout bytes.Buffer
	stderr.Reset()
	if code := run(t.Context(), args, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatal("same selection/path recovery failed", code)
	}
	conflictBytes := []byte("EXISTING_DIFFERENT_OUTPUT_DO_NOT_REPLACE")
	if err := os.WriteFile(output, conflictBytes, 0600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := run(t.Context(), args, &stdout, &stderr); code != 3 || stdout.Len() != 0 || readDiagnostic(t, stderr.Bytes()) != "output_unavailable" {
		t.Fatal("different existing output admitted", code)
	}
	after, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(after, conflictBytes) {
		t.Fatal("conflicting output overwritten")
	}
	assertPrepareSourceConserved(t, snapshot, before)
}

func TestPrepareJobsCLIRejectsUnsafeInputsAndPreservesSource(t *testing.T) {
	snapshot, selection, good := prepareCLIFixture(t, prepareFixtureBrief)
	before, _ := os.ReadFile(snapshot)
	output := filepath.Join(filepath.Dir(snapshot), "never-published.json")
	base := prepareCLIArgs(snapshot, selection, output)
	for name, args := range map[string][]string{
		"missing":            {"prepare-jobs"},
		"duplicate":          append(append([]string{}, base...), "--snapshot", snapshot),
		"duplicate assigned": append(append([]string{}, base...), "--selection="+selection),
		"unknown":            append(append([]string{}, base...), "--private-unknown", "PRIVATE_VALUE"),
		"relative":           {"prepare-jobs", "--snapshot", "relative", "--selection", selection, "--output", output},
		"zero timeout":       append(append([]string{}, base...), "--timeout", "0s"),
		"large timeout":      append(append([]string{}, base...), "--timeout", "6m"),
	} {
		t.Run(name, func(t *testing.T) {
			var out, diagnostic bytes.Buffer
			if code := run(t.Context(), args, &out, &diagnostic); code != 2 || out.Len() != 0 || readDiagnostic(t, diagnostic.Bytes()) != "arguments_invalid" {
				t.Fatal("invalid flags admitted", code)
			}
			assertPrepareSafe(t, diagnostic.Bytes(), snapshot, selection, output, "PRIVATE_VALUE")
		})
	}
	valid, _ := json.Marshal(good)
	for name, data := range map[string][]byte{
		"malformed": []byte("{"), "unknown": append([]byte(`{"unknown":"PRIVATE_VALUE",`), valid[1:]...),
		"duplicate": append([]byte(`{"version":1,`), valid[1:]...), "oversized selection": []byte(strings.Repeat("x", (64<<10)+1)),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(selection, data, 0600); err != nil {
				t.Fatal(err)
			}
			var out, diagnostic bytes.Buffer
			if code := run(t.Context(), base, &out, &diagnostic); code != 3 || out.Len() != 0 || readDiagnostic(t, diagnostic.Bytes()) != "selection_invalid" {
				t.Fatal("invalid selection JSON admitted", code)
			}
			assertPrepareSafe(t, diagnostic.Bytes(), snapshot, selection, output, "PRIVATE_VALUE")
		})
	}
	writePrepareSelection(t, selection, good)
	if runtime.GOOS != "windows" {
		if err := os.Chmod(selection, 0644); err != nil {
			t.Fatal(err)
		}
		var out, diagnostic bytes.Buffer
		if code := run(t.Context(), base, &out, &diagnostic); code != 3 || readDiagnostic(t, diagnostic.Bytes()) != "selection_invalid" || out.Len() != 0 {
			t.Fatal("exposed selection admitted")
		}
		if err := os.Chmod(selection, 0600); err != nil {
			t.Fatal(err)
		}
	}
	good.Workers = map[string]string{}
	writePrepareSelection(t, selection, good)
	var out, diagnostic bytes.Buffer
	if code := run(t.Context(), base, &out, &diagnostic); code != 3 || out.Len() != 0 || readDiagnostic(t, diagnostic.Bytes()) != "snapshot_conversion_blocked" {
		t.Fatal("unsupported worker mapping silently changed", code)
	}
	assertPrepareSafe(t, diagnostic.Bytes(), snapshot, selection, output)
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatal("refusal published output")
	}
	assertPrepareSourceConserved(t, snapshot, before)
	oversized, selected, _ := prepareCLIFixture(t, strings.Repeat("x", 32769))
	original, _ := os.ReadFile(oversized)
	var oversizedOut, oversizedDiagnostic bytes.Buffer
	if code := run(t.Context(), prepareCLIArgs(oversized, selected, filepath.Join(filepath.Dir(oversized), "refused.json")), &oversizedOut, &oversizedDiagnostic); code != 3 || oversizedOut.Len() != 0 || readDiagnostic(t, oversizedDiagnostic.Bytes()) != "snapshot_conversion_blocked" {
		t.Fatal("oversized source brief truncated/admitted", code)
	}
	assertPrepareSourceConserved(t, oversized, original)
}

func TestPrepareJobsCLICancellationAndDeadlineDoNotPublish(t *testing.T) {
	snapshot, selection, _ := prepareCLIFixture(t, prepareFixtureBrief)
	before, _ := os.ReadFile(snapshot)
	output := filepath.Join(filepath.Dir(snapshot), "never-published.json")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out, diagnostic bytes.Buffer
	if code := run(ctx, prepareCLIArgs(snapshot, selection, output), &out, &diagnostic); code != 130 || out.Len() != 0 || readDiagnostic(t, diagnostic.Bytes()) != "preparation_canceled" {
		t.Fatal("cancellation not distinguished", code)
	}
	deadline, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	out.Reset()
	diagnostic.Reset()
	if code := run(deadline, prepareCLIArgs(snapshot, selection, output), &out, &diagnostic); code != 3 || out.Len() != 0 || readDiagnostic(t, diagnostic.Bytes()) != "preparation_timed_out" {
		t.Fatal("deadline not distinguished", code)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatal("interrupted preparation published output")
	}
	assertPrepareSafe(t, diagnostic.Bytes(), snapshot, selection, output)
	assertPrepareSourceConserved(t, snapshot, before)
}
