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
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/coordimportmodel"
	"github.com/benhook1013/FireController/tools/legacyimport/internal/legacy"
)

const privateMessageBody = "  PRIVATE_MESSAGE_BODY 雪\nExact original text  "

func prepareMessageCLIFixture(t *testing.T) (string, string, legacy.MessageSelection) {
	t.Helper()
	snapshot, selection, _ := prepareCLIFixture(t, prepareFixtureBrief)
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(snapshot)}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`INSERT INTO inbox_messages VALUES('PRIVATE_ROOT','PRIVATE_RECIPIENT',?,'PRIVATE_AUTHOR','PRIVATE_SOURCE_JOB',7,NULL,'2024-01-01T00:00:00.123456789+13:00',NULL,NULL); INSERT INTO inbox_messages VALUES('PRIVATE_REPLY','PRIVATE_AUTHOR','PRIVATE_REPLY_BODY','PRIVATE_RECIPIENT',NULL,7,'PRIVATE_ROOT','2024-01-02T00:00:00Z','2024-01-02T01:00:00Z','2024-01-02T02:00:00Z'); INSERT INTO inbox_messages VALUES('PRIVATE_UNKNOWN','PRIVATE_RECIPIENT','PRIVATE_UNKNOWN_BODY',NULL,NULL,NULL,NULL,'2024-01-03T00:00:00Z',NULL,NULL)`, privateMessageBody); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	revision := int64(0)
	s := legacy.MessageSelection{Version: 1, RepositoryID: 71, ExpectedRevision: &revision, Source: "selected-legacy-export", MessageIDs: []string{"PRIVATE_UNKNOWN", "PRIVATE_REPLY", "PRIVATE_ROOT"}, Workers: map[string]string{"PRIVATE_AUTHOR": "worker_author", "PRIVATE_RECIPIENT": "worker_recipient"}, UnknownAuthorWorkerID: "worker_unknown", AssociationPolicy: "archive"}
	writePrepareSelection(t, selection, s)
	return snapshot, selection, s
}
func prepareMessageArgs(snapshot, selection, output string) []string {
	return []string{"prepare-messages", "--snapshot", snapshot, "--selection", selection, "--output", output}
}
func assertMessageOutputSafe(t *testing.T, b []byte, paths ...string) {
	t.Helper()
	assertPrepareSafe(t, b, append(paths, "PRIVATE_ROOT", "PRIVATE_REPLY", "PRIVATE_UNKNOWN", "PRIVATE_RECIPIENT", "PRIVATE_AUTHOR", privateMessageBody, "PRIVATE_REPLY_BODY", "PRIVATE_UNKNOWN_BODY")...)
}
func TestPrepareMessagesCLIPrivateOutputExactReplayAndSafeSummary(t *testing.T) {
	snapshot, selection, _ := prepareMessageCLIFixture(t)
	before, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(filepath.Dir(snapshot), "messages.json")
	args := prepareMessageArgs(snapshot, selection, output)
	var out, diagnostic bytes.Buffer
	if code := run(t.Context(), args, &out, &diagnostic); code != 0 || diagnostic.Len() != 0 {
		t.Fatal("prepare failed", code, diagnostic.String())
	}
	assertMessageOutputSafe(t, out.Bytes(), snapshot, selection, output)
	var summary legacy.MessagePreparationSummary
	sum := sha256.Sum256(before)
	if json.Unmarshal(out.Bytes(), &summary) != nil || summary.Kind != "prepared_message_import" || summary.SourceSHA256 != hex.EncodeToString(sum[:]) || summary.Messages != 3 || summary.Replies != 1 || summary.ArchivedAssociations != 3 || summary.UnknownAuthors != 1 {
		t.Fatal("incorrect compact summary")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(out.Bytes(), &fields) != nil || len(fields) != 6 {
		t.Fatal("summary exposed noncompact fields")
	}
	prepared, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var batch coordimportmodel.Batch
	if json.Unmarshal(prepared, &batch) != nil || len(batch.Jobs) != 0 || batch.Jobs == nil || len(batch.Messages) != 3 || batch.SourceSHA256 != summary.SourceSHA256 {
		t.Fatal("bad native prepared batch")
	}
	if batch.Messages[0].SourceKey != "PRIVATE_ROOT" || batch.Messages[0].Body != privateMessageBody || batch.Messages[0].CreatedAt != "2024-01-01T00:00:00.123456789+13:00" || batch.Messages[1].ReplyToSourceKey != "PRIVATE_ROOT" || batch.Messages[1].AcknowledgedAt != "2024-01-02T02:00:00Z" || batch.Messages[2].SenderWorkerID != "worker_unknown" {
		t.Fatal("message facts changed")
	}
	info, err := os.Lstat(output)
	if err != nil || !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("output is not private regular file")
	}
	out.Reset()
	if code := run(t.Context(), args, &out, &diagnostic); code != 0 || diagnostic.Len() != 0 {
		t.Fatal("exact replay failed", code)
	}
	same, err := os.ReadFile(output)
	now, e := os.Lstat(output)
	if err != nil || e != nil || !bytes.Equal(prepared, same) || !os.SameFile(info, now) {
		t.Fatal("exact replay replaced output")
	}
	assertPrepareSourceConserved(t, snapshot, before)
}
func TestPrepareMessagesCLIRecoveryAndChangedSelectionOrSourceRefusesOverwrite(t *testing.T) {
	snapshot, selection, s := prepareMessageCLIFixture(t)
	originalSelection := s
	output := filepath.Join(filepath.Dir(snapshot), "messages.json")
	args := prepareMessageArgs(snapshot, selection, output)
	var diagnostic, out bytes.Buffer
	if code := run(t.Context(), args, prepareShortWriter{}, &diagnostic); code != 3 || readDiagnostic(t, diagnostic.Bytes()) != "output_unavailable" {
		t.Fatal("lost stdout not reported")
	}
	prepared, err := os.ReadFile(output)
	if err != nil {
		t.Fatal("lost stdout did not preserve prepared file")
	}
	diagnostic.Reset()
	if code := run(t.Context(), args, &out, &diagnostic); code != 0 {
		t.Fatal("recovery failed")
	}
	s.MessageIDs = []string{"PRIVATE_ROOT"}
	writePrepareSelection(t, selection, s)
	diagnostic.Reset()
	out.Reset()
	if code := run(t.Context(), args, &out, &diagnostic); code != 3 || readDiagnostic(t, diagnostic.Bytes()) != "output_unavailable" || out.Len() != 0 {
		t.Fatal("changed selection overwrote output")
	}
	writePrepareSelection(t, selection, originalSelection)
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(snapshot)}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE jobs SET title='Different sealed source title'`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	changedSource, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	diagnostic.Reset()
	out.Reset()
	if code := run(t.Context(), args, &out, &diagnostic); code != 3 || readDiagnostic(t, diagnostic.Bytes()) != "output_unavailable" || out.Len() != 0 {
		t.Fatal("changed source seal overwrote output")
	}
	after, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(prepared, after) {
		t.Fatal("refusal modified original output")
	}
	assertPrepareSourceConserved(t, snapshot, changedSource)
	assertMessageOutputSafe(t, diagnostic.Bytes(), snapshot, selection, output)
}
func TestPrepareMessagesCLIInvalidSelectionFlagsAndPrivateFiles(t *testing.T) {
	snapshot, selection, s := prepareMessageCLIFixture(t)
	before, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(filepath.Dir(snapshot), "never.json")
	base := prepareMessageArgs(snapshot, selection, output)
	for name, args := range map[string][]string{
		"missing": {"prepare-messages"}, "duplicate": append(append([]string{}, base...), "--snapshot", snapshot), "unknown": append(append([]string{}, base...), "--unknown", "PRIVATE_VALUE"), "relative": {"prepare-messages", "--snapshot", "relative", "--selection", selection, "--output", output}, "zero-timeout": append(append([]string{}, base...), "--timeout", "0s"), "large-timeout": append(append([]string{}, base...), "--timeout", "6m"),
	} {
		t.Run(name, func(t *testing.T) {
			var out, diagnostic bytes.Buffer
			if code := run(t.Context(), args, &out, &diagnostic); code != 2 || out.Len() != 0 || readDiagnostic(t, diagnostic.Bytes()) != "arguments_invalid" {
				t.Fatal("invalid arguments admitted")
			}
			assertMessageOutputSafe(t, diagnostic.Bytes(), snapshot, selection, output, "PRIVATE_VALUE")
		})
	}
	valid, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"malformed": []byte("{"), "unknown": append([]byte(`{"unknown":"PRIVATE_VALUE",`), valid[1:]...), "duplicate": append([]byte(`{"version":1,`), valid[1:]...), "oversized": []byte(strings.Repeat("x", (64<<10)+1))} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(selection, data, 0600); err != nil {
				t.Fatal(err)
			}
			var out, diagnostic bytes.Buffer
			if code := run(t.Context(), base, &out, &diagnostic); code != 3 || out.Len() != 0 || readDiagnostic(t, diagnostic.Bytes()) != "selection_invalid" {
				t.Fatal("invalid selection admitted")
			}
			assertMessageOutputSafe(t, diagnostic.Bytes(), snapshot, selection, output)
		})
	}
	writePrepareSelection(t, selection, s)
	if runtime.GOOS != "windows" {
		if err := os.Chmod(selection, 0644); err != nil {
			t.Fatal(err)
		}
		var out, diagnostic bytes.Buffer
		if code := run(t.Context(), base, &out, &diagnostic); code != 3 || readDiagnostic(t, diagnostic.Bytes()) != "selection_invalid" {
			t.Fatal("exposed selection admitted")
		}
		if err := os.Chmod(selection, 0600); err != nil {
			t.Fatal(err)
		}
	}
	s.UnknownAuthorWorkerID = ""
	writePrepareSelection(t, selection, s)
	var out, diagnostic bytes.Buffer
	if code := run(t.Context(), base, &out, &diagnostic); code != 3 || readDiagnostic(t, diagnostic.Bytes()) != "snapshot_conversion_blocked" || out.Len() != 0 {
		t.Fatal("unknown author guessed")
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatal("refusal published output")
	}
	assertPrepareSourceConserved(t, snapshot, before)
}
func TestPrepareMessagesCLICancellationAndHelp(t *testing.T) {
	snapshot, selection, _ := prepareMessageCLIFixture(t)
	before, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(filepath.Dir(snapshot), "never.json")
	canceled, stop := context.WithCancel(t.Context())
	stop()
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	for _, test := range []struct {
		ctx  context.Context
		code int
		kind string
	}{{canceled, 130, "preparation_canceled"}, {expired, 3, "preparation_timed_out"}} {
		var out, diagnostic bytes.Buffer
		if code := run(test.ctx, prepareMessageArgs(snapshot, selection, output), &out, &diagnostic); code != test.code || out.Len() != 0 || readDiagnostic(t, diagnostic.Bytes()) != test.kind {
			t.Fatal("cancellation/deadline not classified")
		}
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatal("interrupted output published")
	}
	assertPrepareSourceConserved(t, snapshot, before)
	var out, diagnostic bytes.Buffer
	if code := run(t.Context(), []string{"prepare-messages", "--help"}, &out, &diagnostic); code != 0 || !strings.Contains(out.String(), "Association policy must be archive") || diagnostic.Len() != 0 {
		t.Fatal("help unavailable")
	}
}

func TestPrepareMessagesCLIInspectionLimitsValidateBeforeInputReads(t *testing.T) {
	dir := t.TempDir()
	snapshot, selection, output := filepath.Join(dir, "PRIVATE_MISSING_SNAPSHOT"), filepath.Join(dir, "PRIVATE_MISSING_SELECTION"), filepath.Join(dir, "never.json")
	base := prepareMessageArgs(snapshot, selection, output)
	var help bytes.Buffer
	if code := run(t.Context(), []string{"prepare-messages", "--help"}, &help, &bytes.Buffer{}); code != 0 {
		t.Fatal("help failed", code)
	}
	for _, name := range []string{"max-database-bytes", "max-rows-per-table", "max-total-rows", "max-cell-bytes", "max-total-cell-bytes"} {
		if !strings.Contains(help.String(), "--"+name+" N") {
			t.Fatalf("help omits %s", name)
		}
		for _, value := range []string{"0", "-1", "PRIVATE_NOT_A_NUMBER", "9223372036854775807", "9223372036854775808"} {
			t.Run(name+"/"+value, func(t *testing.T) {
				args := append(append([]string{}, base...), "--"+name, value)
				var out, diagnostic bytes.Buffer
				if code := run(t.Context(), args, &out, &diagnostic); code != 2 || out.Len() != 0 || readDiagnostic(t, diagnostic.Bytes()) != "arguments_invalid" {
					t.Fatal("invalid limit reached missing input", code)
				}
				assertMessageOutputSafe(t, diagnostic.Bytes(), snapshot, selection, output, value)
			})
		}
		t.Run(name+"/duplicate", func(t *testing.T) {
			args := append(append([]string{}, base...), "--"+name, "100", "--"+name+"=200")
			var out, diagnostic bytes.Buffer
			if code := run(t.Context(), args, &out, &diagnostic); code != 2 || out.Len() != 0 || readDiagnostic(t, diagnostic.Bytes()) != "arguments_invalid" {
				t.Fatal("duplicate limit reached missing input", code)
			}
			assertMessageOutputSafe(t, diagnostic.Bytes(), snapshot, selection, output)
		})
		for _, value := range []string{"1", "9223372036854775806"} {
			t.Run(name+"/positive/"+value, func(t *testing.T) {
				args := append(append([]string{}, base...), "--"+name+"="+value)
				var out, diagnostic bytes.Buffer
				if code := run(t.Context(), args, &out, &diagnostic); code != 3 || out.Len() != 0 || readDiagnostic(t, diagnostic.Bytes()) != "selection_invalid" {
					t.Fatal("valid positive limit did not proceed to selected input", code)
				}
				assertMessageOutputSafe(t, diagnostic.Bytes(), snapshot, selection, output)
			})
		}
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatal("argument validation published output")
	}
}

func TestPrepareMessagesCLIInspectionOverridesAndReplayConserveSource(t *testing.T) {
	snapshot, selection, _ := prepareMessageCLIFixture(t)
	// Only unselected source data exceeds the default; selected messages retain
	// their original native limits and content under every admission override.
	largeCell := legacy.DefaultLimits().MaxCellBytes + 1
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(snapshot)}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(t.Context(), `INSERT INTO job_updates(job_id,created_at,kind,body) VALUES('PRIVATE_SOURCE_JOB','2024-02-03T04:05:06Z','progress',?)`, strings.Repeat("x", int(largeCell)))
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatal("synthetic oversized update", err, closeErr)
	}
	before, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	refuse := func(args []string, output string) {
		t.Helper()
		var out, diagnostic bytes.Buffer
		if code := run(t.Context(), args, &out, &diagnostic); code != 3 || out.Len() != 0 || readDiagnostic(t, diagnostic.Bytes()) != "snapshot_too_large" {
			t.Fatal("inspection bound not enforced", code, diagnostic.String())
		}
		assertMessageOutputSafe(t, diagnostic.Bytes(), snapshot, selection, output)
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			t.Fatal("limit refusal published output")
		}
	}
	defaultOutput := filepath.Join(filepath.Dir(snapshot), "default-refused.json")
	refuse(prepareMessageArgs(snapshot, selection, defaultOutput), defaultOutput)
	var canonicalPrepared []byte
	for name, raised := range map[string]int64{
		"max-database-bytes":   int64(len(before)),
		"max-rows-per-table":   100,
		"max-total-rows":       1000,
		"max-cell-bytes":       largeCell,
		"max-total-cell-bytes": largeCell + (1 << 20),
	} {
		t.Run(name, func(t *testing.T) {
			output := filepath.Join(filepath.Dir(snapshot), name+".json")
			base := prepareMessageArgs(snapshot, selection, output)
			if name != "max-cell-bytes" {
				base = append(base, "--max-cell-bytes", strconv.FormatInt(largeCell, 10))
			}
			refuse(append(append([]string{}, base...), "--"+name, "1"), output)
			args := append(append([]string{}, base...), "--"+name+"="+strconv.FormatInt(raised, 10))
			var out, diagnostic bytes.Buffer
			if code := run(t.Context(), args, &out, &diagnostic); code != 0 || diagnostic.Len() != 0 {
				t.Fatal("selected sufficient limit failed", code, diagnostic.String())
			}
			assertMessageOutputSafe(t, out.Bytes(), snapshot, selection, output)
			prepared, err := os.ReadFile(output)
			var batch coordimportmodel.Batch
			if err != nil || json.Unmarshal(prepared, &batch) != nil || len(batch.Jobs) != 0 || len(batch.Messages) != 3 || batch.Messages[0].Body != privateMessageBody || batch.Messages[1].ReplyToSourceKey != "PRIVATE_ROOT" {
				t.Fatal("inspection override changed selected native content", err)
			}
			if canonicalPrepared == nil {
				canonicalPrepared = prepared
			} else if !bytes.Equal(canonicalPrepared, prepared) {
				t.Fatal("admission-only limits changed prepared bytes")
			}
			info, err := os.Lstat(output)
			if err != nil {
				t.Fatal(err)
			}
			out.Reset()
			replay := append(append([]string{}, base...), "--"+name, strconv.FormatInt(raised+1, 10))
			if code := run(t.Context(), replay, &out, &diagnostic); code != 0 || diagnostic.Len() != 0 {
				t.Fatal("bound-only exact replay failed", code, diagnostic.String())
			}
			same, err := os.ReadFile(output)
			now, statErr := os.Lstat(output)
			if err != nil || statErr != nil || !bytes.Equal(prepared, same) || !os.SameFile(info, now) {
				t.Fatal("bound-only replay replaced output")
			}
		})
	}
	assertPrepareSourceConserved(t, snapshot, before)
}
