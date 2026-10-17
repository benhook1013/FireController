package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/backup"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviewruntime"
	"github.com/benhook1013/FireController/internal/reviews"
	"github.com/benhook1013/FireController/internal/server"
)

type activationCommandFixture struct {
	dir, instance, predecessor, oldAdmin, oldClient, token, tokenFile, project, worker string
}

func activationHTTP(t *testing.T, base, method, path, token, instance, worker string, body any) (int, map[string]any) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequestWithContext(t.Context(), method, base+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-FC-Instance", instance)
	r.Header.Set("X-FC-Worker", worker)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", core.NewID("activation-fixture"))
	r.Header.Set("Prefer", "wait=0")
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if json.Unmarshal(b, &result) != nil {
		t.Fatal("non-JSON response", resp.StatusCode)
	}
	return resp.StatusCode, result
}

func newActivationCommandFixture(t *testing.T) activationCommandFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "source")
	s, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, ensure := range []func(context.Context, *core.Store) error{jobs.EnsureSchema, inbox.EnsureSchema, reviews.EnsureSchema, reviewexec.EnsureSchema} {
		if err = ensure(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	adminBytes, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	oldAdmin := strings.TrimSpace(string(adminBytes))
	e, err := reviewexec.New(s, []reviewexec.Capability{{ID: "activation-cap", Provider: "coderabbit", Account: "synthetic-account", Channels: []string{"cli", "hosted"}}, {ID: "activation-cap-next", Provider: "coderabbit", Account: "synthetic-account-next", Channels: []string{"cli", "hosted"}}})
	if err != nil {
		t.Fatal(err)
	}
	mux := server.New(s, server.WithRepositoryResolver(func(context.Context, string) (server.RepositoryIdentity, error) {
		return server.RepositoryIdentity{ID: 991, FullName: "synthetic/activation"}, nil
	}))
	jobs.Register(mux, s)
	inbox.Register(mux, s)
	e.Register(mux)
	host := httptest.NewServer(mux)
	defer host.Close()
	status, p := activationHTTP(t, host.URL, "POST", "/api/v1/projects", oldAdmin, s.InstanceID(), "", map[string]any{"name": "Retained project", "repository": "synthetic/activation"})
	if status != 200 {
		t.Fatal(status, p)
	}
	project := p["id"].(string)
	base := "/api/v1/projects/" + project
	status, w := activationHTTP(t, host.URL, "POST", base+"/workers", oldAdmin, s.InstanceID(), "", map[string]any{"name": "Retained worker"})
	if status != 200 {
		t.Fatal(status, w)
	}
	worker := w["id"].(string)
	oldClient := "synthetic-predecessor-client-credential-0000"
	hash := sha256.Sum256([]byte(oldClient))
	status, v := activationHTTP(t, host.URL, "POST", base+"/credentials", oldAdmin, s.InstanceID(), worker, map[string]any{"token_sha256": hex.EncodeToString(hash[:]), "permissions": []string{"read", "write"}})
	if status != 200 {
		t.Fatal(status, v)
	}
	req := httptest.NewRequest("POST", "/", nil)
	req.SetPathValue("project_id", project)
	req.Header.Set("Authorization", "Bearer "+oldAdmin)
	req.Header.Set("X-FC-Instance", s.InstanceID())
	req.Header.Set("X-FC-Worker", worker)
	scope, err := s.AdminScope(req)
	if err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	if _, err = e.Configure(t.Context(), scope, "activation-config", reviewexec.Configuration{ExpectedRevision: &zero, CapabilityID: "activation-cap", Mode: "independent", Queue: []int64{1, 2, 3, 4, 5}, Channels: []string{"cli", "hosted"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.ConfigurePolicy(t.Context(), scope, "activation-policy", reviewexec.PolicyConfiguration{ExpectedRevision: &zero, RequiredChannels: []string{"cli", "hosted"}, PausedChannels: []string{}, MaxReviewFiles: 0}); err != nil {
		t.Fatal(err)
	}
	configurationRevision := int64(1)
	enqueue := func(number int64, channel string) reviewexec.Receipt {
		b, err := e.Enqueue(t.Context(), scope, core.NewID("fixture"), reviewexec.Request{ExpectedConfigurationRevision: configurationRevision, PullNumber: number, Channel: channel, HeadSHA: strings.Repeat("a", 40)})
		if err != nil {
			t.Fatal(err)
		}
		var receipt reviewexec.Receipt
		if json.Unmarshal(b, &receipt) != nil {
			t.Fatal("request receipt")
		}
		return receipt
	}
	anchor := func(number int64) reviewexec.Anchor {
		return reviewexec.Anchor{RepositoryID: 991, Repository: "synthetic/activation", PullNumber: number, BaseRef: "main", CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), ParentSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("b", 40), PatchSHA256: strings.Repeat("c", 64), ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	}
	completeOp := enqueue(4, "cli")
	a, err := e.StartAttempt(t.Context(), project, completeOp.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, err = e.Admit(t.Context(), project, a.ID, a.Revision, anchor(4), "")
	if err != nil {
		t.Fatal(err)
	}
	captures, err := reviewruntime.NewCaptureStore(filepath.Join(dir, "review-captures"))
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC()
	reference, err := captures.Save(t.Context(), a.ID, reviewruntime.CaptureRecord{Capture: reviewprocess.Capture{Started: true, Stdout: []byte("synthetic completed capture")}, StartedAt: stamp, FinishedAt: stamp})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := e.RecordReviewEvidence(t.Context(), project, a.ID, reviewexec.ReviewEvidenceInput{CaptureID: reference.ID, CaptureSHA256: reference.SHA256, CaptureBytes: reference.Bytes, StartedAt: stamp.Format(time.RFC3339Nano), FinishedAt: stamp.Format(time.RFC3339Nano), ExpectedAnchor: *a.Anchor, Review: reviewprovider.ParsedReview{Outcome: "completed", Complete: true, Findings: []reviewprovider.ParsedFinding{}, ReviewedFiles: []string{"public.go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Advance(t.Context(), project, a.ID, a.Revision, "completed", "capture_completed", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = e.FinalizeReviewEvidence(t.Context(), scope, "activation-finalize", evidence.ID, reviewexec.FinalizeReviewRequest{ExpectedRevision: &zero}); err != nil {
		t.Fatal(err)
	}
	one := int64(1)
	if _, err = e.RequestCheckpointPublication(t.Context(), scope, "activation-publication", reviewexec.CheckpointPublicationRequest{EvidenceID: evidence.ID, ExpectedRevision: &one}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.EnqueueSourceRefresh(t.Context(), scope, "activation-source", reviewexec.SourceRefreshRequest{ExpectedConfigurationRevision: 1, PullNumber: 5, HeadSHA: strings.Repeat("a", 40)}); err != nil {
		t.Fatal(err)
	}
	_ = enqueue(1, "cli")
	preparing := enqueue(2, "cli")
	if _, err = e.StartAttempt(t.Context(), project, preparing.ID); err != nil {
		t.Fatal(err)
	}
	// A second retained account permits a separate uncertain action while the
	// original account still owns its preparing reservation.
	if _, err = e.Configure(t.Context(), scope, "activation-config-next", reviewexec.Configuration{ExpectedRevision: &one, CapabilityID: "activation-cap-next", Mode: "independent", Queue: []int64{1, 2, 3, 4, 5}, Channels: []string{"cli", "hosted"}}); err != nil {
		t.Fatal(err)
	}
	configurationRevision = 2
	uncertainOp := enqueue(3, "hosted")
	a, err = e.StartAttempt(t.Context(), project, uncertainOp.ID)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	a, err = e.Admit(t.Context(), project, a.ID, a.Revision, anchor(3), intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Advance(t.Context(), project, a.ID, a.Revision, "uncertain", "dispatch_unknown", ""); err != nil {
		t.Fatal(err)
	}
	predecessor := s.InstanceID()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	var bundle backup.Summary
	if json.Unmarshal(runBackupResult(t, "backup", "--data-dir", dir, "--destination", filepath.Join(t.TempDir(), "bundles")), &bundle) != nil {
		t.Fatal("backup result")
	}
	resultDir := filepath.Join(t.TempDir(), "restored")
	var restored backup.RestoreSummary
	if json.Unmarshal(runBackupResult(t, "restore", "--bundle", bundle.Path, "--data-dir", resultDir), &restored) != nil {
		t.Fatal("restore result")
	}
	token := "synthetic-new-activation-admin-credential-0000"
	tokenFile := filepath.Join(t.TempDir(), "private-admin.token")
	if err = os.WriteFile(tokenFile, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return activationCommandFixture{dir: resultDir, instance: restored.InstanceID, predecessor: predecessor, oldAdmin: oldAdmin, oldClient: oldClient, token: token, tokenFile: tokenFile, project: project, worker: worker}
}

func (f activationCommandFixture) args() []string {
	return []string{"restore-activate", "--data-dir", f.dir, "--expect-instance", f.instance, "--fenced-predecessor", f.predecessor, "--admin-token-file", f.tokenFile}
}

func activationWorkRows(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "firecontroller.sqlite"))+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	result := map[string][]byte{}
	for _, table := range []string{"review_operations", "review_execution_attempts", "review_execution_reservations", "review_execution_transitions", "review_source_refreshes", "review_checkpoint_publications", "review_checkpoint_revisions", "review_checkpoint_intents", "review_execution_evidence"} {
		rows, err := db.Query("SELECT * FROM " + table + " ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var records [][]any
		for rows.Next() {
			v := make([]any, len(cols))
			dest := make([]any, len(v))
			for i := range v {
				dest[i] = &v[i]
			}
			if err = rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			records = append(records, v)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		result[table], err = json.Marshal(records)
		if err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func activationStart(t *testing.T, f activationCommandFixture, options ...string) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	out := &output{ready: make(chan struct{})}
	diagnostics := &output{ready: make(chan struct{})}
	done := make(chan int, 1)
	args := []string{"--data-dir", f.dir, "--listen", "127.0.0.1:0", "--review-catalog", filepath.Join(f.dir, "must-not-read-catalog"), "--github-token-file", filepath.Join(f.dir, "must-not-read-token")}
	args = append(args, options...)
	go func() { done <- run(ctx, args, out, diagnostics) }()
	select {
	case <-out.ready:
	case code := <-done:
		cancel()
		t.Fatalf("held startup failed: %d %s", code, diagnostics.String())
	case <-time.After(8 * time.Second):
		cancel()
		t.Fatal("held startup deadline")
	}
	var start map[string]string
	if json.Unmarshal([]byte(out.String()), &start) != nil || start["instance_id"] != f.instance {
		cancel()
		t.Fatal("held startup identity")
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case code := <-done:
			if code != 0 || diagnostics.String() != "" {
				t.Fatalf("held shutdown: %d %s", code, diagnostics.String())
			}
		case <-time.After(8 * time.Second):
			t.Fatal("held shutdown deadline")
		}
	}
	t.Cleanup(stop)
	return "http://" + start["address"], stop
}

func TestRestoreActivationCommandCoordinationAndHeldRestart(t *testing.T) {
	f := newActivationCommandFixture(t)
	first := runBackupResult(t, f.args()...)
	var result backup.ActivationSummary
	if json.Unmarshal(first, &result) != nil || result.State != "coordination_active" || !result.ProvidersHeld || result.InstanceID != f.instance {
		t.Fatal("activation summary")
	}
	if !bytes.Equal(first, runBackupResult(t, f.args()...)) {
		t.Fatal("exact activation retry changed identity/receipt")
	}
	before := activationWorkRows(t, f.dir)
	for restart := 0; restart < 2; restart++ {
		base, stop := activationStart(t, f)
		if !reflect.DeepEqual(before, activationWorkRows(t, f.dir)) {
			t.Fatal("held boot recovered or changed retained provider work")
		}
		status, v := activationHTTP(t, base, "GET", "/readyz", f.token, f.instance, f.worker, nil)
		if status != 200 {
			t.Fatal(status, v)
		}
		status, v = activationHTTP(t, base, "GET", "/api/v1/instance", f.token, f.instance, f.worker, nil)
		if status != 200 || v["instance_id"] != f.instance {
			t.Fatal("new administrator unavailable", status)
		}
		for _, old := range []string{f.oldAdmin, f.oldClient} {
			status, _ = activationHTTP(t, base, "GET", "/api/v1/instance", old, f.instance, f.worker, nil)
			if status != 401 {
				t.Fatal("restored old credential accepted", status)
			}
		}
		projectBase := "/api/v1/projects/" + f.project
		newToken := "synthetic-post-activation-worker-credential-" + strings.Repeat("x", restart+1)
		hash := sha256.Sum256([]byte(newToken))
		status, v = activationHTTP(t, base, "POST", projectBase+"/credentials", f.token, f.instance, f.worker, map[string]any{"token_sha256": hex.EncodeToString(hash[:]), "permissions": []string{"read", "write"}})
		if status != 200 {
			t.Fatal("new project client provisioning", status, v)
		}
		status, v = activationHTTP(t, base, "POST", projectBase+"/jobs", newToken, f.instance, f.worker, map[string]any{"title": "Recovered coordination", "brief": "New locally authorized work", "worker_id": f.worker, "status": "active"})
		if status != 201 {
			t.Fatal("coordination job", status, v)
		}
		status, v = activationHTTP(t, base, "POST", projectBase+"/messages", newToken, f.instance, f.worker, map[string]any{"recipient_worker_id": f.worker, "body": "Recovered message"})
		if status != 201 {
			t.Fatal("coordination message", status, v)
		}
		status, v = activationHTTP(t, base, "POST", projectBase+"/reviews/requests", f.token, f.instance, f.worker, map[string]any{"expected_configuration_revision": 2, "pull_number": 5, "channel": "cli", "head_sha": strings.Repeat("a", 40)})
		if status != 409 || v["error"].(map[string]any)["code"] != "capability_unavailable" {
			t.Fatal("held new provider request admitted", status, v)
		}
		status, v = activationHTTP(t, base, "POST", "/api/v1/projects", f.token, f.instance, "", map[string]any{"name": "No provider resolver", "repository": "synthetic/new"})
		if status != 503 {
			t.Fatal("held boot supplied provider resolver", status, v)
		}
		stop()
		if !reflect.DeepEqual(before, activationWorkRows(t, f.dir)) {
			t.Fatal("held lifetime changed retained provider journals")
		}
		for _, name := range []string{"review-workspaces", "hosted-review-captures", "bootstrap-admin.token"} {
			if _, err := os.Lstat(filepath.Join(f.dir, name)); !os.IsNotExist(err) {
				t.Fatal("held boot created provider/bootstrap state", name, err)
			}
		}
	}
}

type activationFailWriter struct{}

func (activationFailWriter) Write([]byte) (int, error) {
	return 0, errors.New("private output failure")
}

func TestRestoreActivationOutputFailureIsExactRetryUncertainty(t *testing.T) {
	f := newActivationCommandFixture(t)
	var diagnostics bytes.Buffer
	if code := run(t.Context(), f.args(), activationFailWriter{}, &diagnostics); code != 3 || !strings.Contains(diagnostics.String(), `"outcome_unknown":true`) || !strings.Contains(diagnostics.String(), "exact same command") || strings.Contains(diagnostics.String(), "private output failure") {
		t.Fatal("activation outcome hidden", code, diagnostics.String())
	}
	var summary backup.ActivationSummary
	if json.Unmarshal(runBackupResult(t, f.args()...), &summary) != nil || summary.State != "coordination_active" {
		t.Fatal("exact recovery did not reuse activation")
	}
}

func TestRestoreActivationArgumentsAndMalformedStateFailBeforeConfig(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	valid := []string{"restore-activate", "--data-dir", root, "--expect-instance", "instance_" + strings.Repeat("a", 32), "--fenced-predecessor", "instance_" + strings.Repeat("b", 32), "--admin-token-file", filepath.Join(root, "admin.token")}
	cases := [][]string{{"restore-activate"}, append(append([]string(nil), valid...), "--expect-instance", "duplicate"), append(append([]string(nil), valid...), "--timeout", "0"), append(append([]string(nil), valid...), "--timeout", "24h1ns"), append(append([]string(nil), valid...), "--unknown", "private-input"), append(append([]string(nil), valid...), "extra"), append(append([]string(nil), valid...), "--timeout", "1s", "--timeout=2s")}
	for _, flag := range []string{"--data-dir", "--expect-instance", "--fenced-predecessor", "--admin-token-file"} {
		var args []string
		for i := 0; i < len(valid); i++ {
			if valid[i] == flag {
				i++
				continue
			}
			args = append(args, valid[i])
		}
		cases = append(cases, args)
	}
	for _, value := range []string{"", "\xff", "bad\x00value"} {
		args := append([]string(nil), valid...)
		args[4] = value
		cases = append(cases, args)
	}
	for _, args := range cases {
		var out, err bytes.Buffer
		if code := run(t.Context(), args, &out, &err); code != 2 || out.Len() != 0 || !json.Valid(err.Bytes()) || strings.Contains(err.String(), root) || strings.Contains(err.String(), "private-input") {
			t.Fatal("invalid activation arguments touched operation", code, err.String())
		}
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatal("invalid activation arguments created filesystem state")
	}
	f := newActivationCommandFixture(t)
	runBackupResult(t, f.args()...)
	db, err := sql.Open("sqlite", filepath.Join(f.dir, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE metadata SET value='unsupported-state' WHERE key='restore:state'`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	var out, diagnostics bytes.Buffer
	if code := run(t.Context(), []string{"--data-dir", f.dir, "--listen", "127.0.0.1:0", "--review-catalog", filepath.Join(root, "absent-catalog"), "--github-token-file", filepath.Join(root, "absent-token")}, &out, &diagnostics); code != 3 || out.Len() != 0 || !strings.Contains(diagnostics.String(), `"code":"restore_inactive"`) {
		t.Fatal("malformed recovery state passed config/listener boundary", code, diagnostics.String())
	}
}
