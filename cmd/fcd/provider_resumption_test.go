package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/backup"
)

func resumptionCommandArgs(f activationCommandFixture) []string {
	return []string{"restore-resume-providers", "--data-dir", f.dir, "--expect-instance", f.instance, "--fenced-predecessor", f.predecessor, "--provider-effects-settled", "--abandon-retained-work=true", "--reason", "  --literal <settled> & résumé  ", "--evidence-reference", "private-proof://literal?value=<safe>&other=1"}
}

func TestRestoreResumeProvidersArgumentsBeforeFilesystem(t *testing.T) {
	f := activationCommandFixture{dir: filepath.Join(t.TempDir(), "must-not-create-private"), instance: "instance_" + strings.Repeat("a", 32), predecessor: "instance_" + strings.Repeat("b", 32)}
	valid := resumptionCommandArgs(f)
	cases := [][]string{{"restore-resume-providers"}}
	for _, extra := range [][]string{{"extra-private"}, {"--"}, {"--unknown=private-value"}, {"--provider-effects-settled"}, {"--timeout=0"}, {"--timeout=-1s"}, {"--timeout=24h1ns"}, {"--timeout=private-value"}, {"--timeout=1s", "--timeout=2s"}, {"--data-dir=duplicate-private"}, {"--github-token-file=must-not-load"}, {"--review-catalog=must-not-load"}} {
		cases = append(cases, append(append([]string(nil), valid...), extra...))
	}
	for _, required := range []string{"--data-dir", "--expect-instance", "--fenced-predecessor", "--provider-effects-settled", "--abandon-retained-work=true", "--reason", "--evidence-reference"} {
		var args []string
		for i := 0; i < len(valid); i++ {
			if valid[i] == required {
				if required != "--provider-effects-settled" && required != "--abandon-retained-work=true" {
					i++
				}
				continue
			}
			args = append(args, valid[i])
		}
		cases = append(cases, args)
	}
	for _, field := range []string{"--data-dir", "--expect-instance", "--fenced-predecessor", "--reason", "--evidence-reference"} {
		for _, value := range []string{"", "  ", "private\nvalue", "private\tvalue", "private\rvalue", "private\x00value", "private\xffvalue"} {
			args := append([]string(nil), valid...)
			for i := range args {
				if args[i] == field {
					args[i+1] = value
				}
			}
			cases = append(cases, args)
		}
	}
	for _, argument := range []string{"--provider-effects-settled=false", "--provider-effects-settled=1", "--provider-effects-settled=TRUE", "--provider-effects-settled=", "---provider-effects-settled"} {
		args := append([]string(nil), valid...)
		args[7] = argument
		cases = append(cases, args)
	}
	for _, argument := range []string{"--abandon-retained-work=false", "--abandon-retained-work=1"} {
		args := append([]string(nil), valid...)
		args[8] = argument
		cases = append(cases, args)
	}
	for _, field := range []string{"--expect-instance", "--fenced-predecessor", "--reason", "--evidence-reference"} {
		args := append([]string(nil), valid...)
		for i := range args {
			if args[i] == field {
				args[i+1] = strings.Repeat("x", 257)
			}
		}
		cases = append(cases, args)
	}
	same := append([]string(nil), valid...)
	same[6] = f.instance
	cases = append(cases, same)
	for i, args := range cases {
		var out, diagnostics bytes.Buffer
		if code := run(t.Context(), args, &out, &diagnostics); code != 2 || out.Len() != 0 || !json.Valid(diagnostics.Bytes()) || !strings.Contains(diagnostics.String(), `"code":"configuration_invalid"`) || strings.Contains(diagnostics.String(), "private-value") || strings.Contains(diagnostics.String(), f.dir) || strings.Contains(diagnostics.String(), "private-proof") {
			t.Fatalf("argument case %d did not fail safely before operation: code=%d diagnostic=%s", i, code, diagnostics.String())
		}
	}
	if _, err := os.Lstat(f.dir); !os.IsNotExist(err) {
		t.Fatal("invalid arguments initialized destination", err)
	}
}

func TestRestoreResumeProvidersCommandReleaseReplayAndStartup(t *testing.T) {
	f := newActivationCommandFixture(t)
	runBackupResult(t, f.args()...)
	original := activationWorkRows(t, f.dir)
	// Held startup ignores deliberately absent provider configuration, without
	// recovering queued/preparing/uncertain provider work.
	_, stop := activationStart(t, f)
	stop()
	if !reflect.DeepEqual(original, activationWorkRows(t, f.dir)) {
		t.Fatal("held startup changed retained provider work")
	}
	first := runBackupResult(t, resumptionCommandArgs(f)...)
	var summary backup.ProviderResumptionSummary
	if json.Unmarshal(first, &summary) != nil || summary.State != "coordination_active" || summary.ProvidersHeld || summary.InstanceID != f.instance || summary.SourceInstanceID != f.predecessor || summary.ReleasedAt == "" || summary.Counts.QueuedOperations != 1 || summary.Counts.Attempts != 2 || summary.Counts.SourceRefreshes != 1 || summary.Counts.Publications != 1 || summary.Counts.PausedProjects != 1 {
		t.Fatal("operator did not return exact compact reconciliation counts", summary)
	}
	if !bytes.Equal(original["review_execution_evidence"], activationWorkRows(t, f.dir)["review_execution_evidence"]) {
		t.Fatal("operator changed authoritative evidence")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(f.dir, "firecontroller.sqlite"))+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var reason, reference string
	err = db.QueryRow(`SELECT json_extract(value,'$.request.reason'),json_extract(value,'$.request.evidence_reference') FROM metadata WHERE key='restore:provider_release'`).Scan(&reason, &reference)
	db.Close()
	if err != nil || reason != "  --literal <settled> & résumé  " || reference != "private-proof://literal?value=<safe>&other=1" || bytes.Contains(first, []byte(reason)) || bytes.Contains(first, []byte(reference)) {
		t.Fatal("operator changed literal private values or exposed them in summary", err)
	}
	retained := resumptionCommandSnapshot(t, f.dir)
	if !bytes.Equal(first, runBackupResult(t, resumptionCommandArgs(f)...)) || !reflect.DeepEqual(retained, resumptionCommandSnapshot(t, f.dir)) {
		t.Fatal("exact retry rewrote access, configuration or work")
	}
	changed := append([]string(nil), resumptionCommandArgs(f)...)
	changed[len(changed)-1] += " changed"
	var out, diagnostics bytes.Buffer
	if code := run(t.Context(), changed, &out, &diagnostics); code != 3 || out.Len() != 0 || !strings.Contains(diagnostics.String(), `"code":"restore_conflict"`) || !reflect.DeepEqual(retained, resumptionCommandSnapshot(t, f.dir)) {
		t.Fatal("changed release request did not conflict safely", code, diagnostics.String())
	}
	// Release does not silently use cached provider configuration. Fresh supplied
	// configuration is observed at startup, before a listener is opened.
	out.Reset()
	diagnostics.Reset()
	if code := run(t.Context(), []string{"--data-dir", f.dir, "--listen", "127.0.0.1:0", "--review-catalog", filepath.Join(f.dir, "must-not-read-catalog")}, &out, &diagnostics); code != 3 || out.Len() != 0 || !strings.Contains(diagnostics.String(), `"code":"configuration_invalid"`) {
		t.Fatal("resumed startup did not load fresh provider configuration", code, diagnostics.String())
	}
	// A no-catalog restart can serve coordination. Exact operator replay must
	// preserve genuinely new current-instance work created after release.
	base, stop := activationStart(t, f, "--review-catalog", "", "--github-token-file", "")
	status, result := activationHTTP(t, base, "POST", "/api/v1/projects/"+f.project+"/jobs", f.token, f.instance, f.worker, map[string]any{"title": "Post-release work", "brief": "Current instance only"})
	if status != 201 {
		t.Fatal("released coordination unavailable", status, result)
	}
	stop()
	retained = resumptionCommandSnapshot(t, f.dir)
	if !bytes.Equal(first, runBackupResult(t, resumptionCommandArgs(f)...)) || !reflect.DeepEqual(retained, resumptionCommandSnapshot(t, f.dir)) {
		t.Fatal("replayed release changed new current-instance work")
	}
}

type resumptionShortWriter struct{}

func (resumptionShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestRestoreResumeProvidersOutputFailureExactRetry(t *testing.T) {
	for _, mode := range []string{"error", "short"} {
		t.Run(mode, func(t *testing.T) {
			f := newActivationCommandFixture(t)
			runBackupResult(t, f.args()...)
			args := resumptionCommandArgs(f)
			var output io.Writer = activationFailWriter{}
			if mode == "short" {
				output = resumptionShortWriter{}
				args[7], args[8] = "--provider-effects-settled=true", "--abandon-retained-work"
			}
			var diagnostics bytes.Buffer
			if code := run(t.Context(), args, output, &diagnostics); code != 3 || !strings.Contains(diagnostics.String(), `"code":"output_failed"`) || !strings.Contains(diagnostics.String(), `"outcome_unknown":true`) || !strings.Contains(diagnostics.String(), "exact same command") || strings.Contains(diagnostics.String(), f.dir) || strings.Contains(diagnostics.String(), "private-proof") || strings.Contains(diagnostics.String(), "private output failure") {
				t.Fatal("committed result loss did not expose safe exact-retry guard", code, diagnostics.String())
			}
			retained := resumptionCommandSnapshot(t, f.dir)
			var summary backup.ProviderResumptionSummary
			if json.Unmarshal(runBackupResult(t, args...), &summary) != nil || summary.ProvidersHeld || summary.ReleasedAt == "" || !reflect.DeepEqual(retained, resumptionCommandSnapshot(t, f.dir)) {
				t.Fatal("failed output did not commit exactly once/historically replay")
			}
		})
	}
}

func TestRestoreResumeProvidersRefusesInactiveOrdinaryAndCancelled(t *testing.T) {
	for _, state := range []string{"inactive", "ordinary"} {
		t.Run(state, func(t *testing.T) {
			var f activationCommandFixture
			if state == "inactive" {
				f = newActivationCommandFixture(t)
			} else {
				f.dir, f.instance = backupCommandSource(t)
				f.predecessor = "instance_" + strings.Repeat("f", 32)
			}
			before := resumptionCommandSnapshot(t, f.dir)
			var out, diagnostics bytes.Buffer
			if code := run(t.Context(), resumptionCommandArgs(f), &out, &diagnostics); code != 3 || out.Len() != 0 || !json.Valid(diagnostics.Bytes()) || strings.Contains(diagnostics.String(), f.dir) || !reflect.DeepEqual(before, resumptionCommandSnapshot(t, f.dir)) {
				t.Fatal("operator changed an unsupported installation", code, diagnostics.String())
			}
		})
	}
	f := activationCommandFixture{dir: filepath.Join(t.TempDir(), "never-initialized"), instance: "instance_" + strings.Repeat("a", 32), predecessor: "instance_" + strings.Repeat("b", 32)}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out, diagnostics bytes.Buffer
	if code := run(ctx, resumptionCommandArgs(f), &out, &diagnostics); code != 3 || out.Len() != 0 || !json.Valid(diagnostics.Bytes()) {
		t.Fatal("cancelled command continued", code, diagnostics.String())
	}
	if _, err := os.Lstat(f.dir); !os.IsNotExist(err) {
		t.Fatal("cancelled command initialized storage", err)
	}
}

// Include access/configuration and coordination rows in addition to the existing
// provider journal fixture snapshot; replay must not touch either family.
func resumptionCommandSnapshot(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	result := activationWorkRows(t, dir)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "firecontroller.sqlite"))+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, table := range []string{"metadata", "credentials", "grants", "receipts", "events", "review_execution_config", "review_hosted_allowance_queries", "jobs", "job_history", "inbox_messages"} {
		rows, err := db.Query("SELECT * FROM " + table + " ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var values [][]any
		for rows.Next() {
			row := make([]any, len(cols))
			dest := make([]any, len(cols))
			for i := range row {
				dest[i] = &row[i]
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			values = append(values, row)
		}
		if rows.Err() != nil {
			t.Fatal(rows.Err())
		}
		rows.Close()
		result[table], err = json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
	}
	return result
}
