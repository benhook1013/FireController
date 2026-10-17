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
