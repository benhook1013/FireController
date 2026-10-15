package reviewruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

type cliEvidenceFixture struct {
	storeDir string
	store    *core.Store
	engine   *reviewexec.Engine
	captures *CaptureStore
	manager  *reviewworkspace.Manager
	admin    string
	writer   string
	reader   string
	project  string
	baseSHA  string
	headSHA  string
}

func newCLIEvidenceFixture(t *testing.T) *cliEvidenceFixture {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is required for the isolated workspace proof")
	}
	root := t.TempDir()
	storeDir := filepath.Join(root, "private-store")
	store, err := core.Open(context.Background(), storeDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := reviewexec.EnsureSchema(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if err := reviewexec.EnsureEvidenceSchema(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	adminData, err := os.ReadFile(filepath.Join(storeDir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	f := &cliEvidenceFixture{storeDir: storeDir, store: store, admin: strings.TrimSpace(string(adminData)), writer: "runtime-evidence-writer-test-token-000000", reader: "runtime-evidence-reader-test-token-000000", project: "runtime-evidence-project"}
	if _, err := store.DB().Exec(`INSERT INTO projects(id,name,github_repository_id,repository) VALUES(?,?,?,?)`, f.project, f.project, 901, "owner/repo"); err != nil {
		t.Fatal(err)
	}
	f.addCredential(t, "runtime-evidence-writer", "runtime-evidence-writer-principal", f.writer, true)
	f.addCredential(t, "runtime-evidence-reader", "runtime-evidence-reader-principal", f.reader, false)
	// This capture fixture starts with an explicit synthetic administrator policy;
	// policy configuration events and receipts belong to their dedicated tests.
	if _, err := store.DB().Exec(`INSERT INTO review_policy_config(project_id,revision,body_json,actor_id,created_at) VALUES(?,1,?,'synthetic-runtime-admin',?)`, f.project, `{"required_channels":["cli","hosted"],"paused_channels":[],"max_review_files":0}`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	f.engine, err = reviewexec.New(store, []reviewexec.Capability{{ID: "runtime-capability", Provider: "coderabbit", Account: "runtime-account", Channels: []string{"cli", "hosted"}}})
	if err != nil {
		t.Fatal(err)
	}
	f.captures, err = NewCaptureStore(filepath.Join(root, "capture-artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	f.manager, err = reviewworkspace.New(filepath.Join(root, "workspaces"), git)
	if err != nil {
		t.Fatal(err)
	}
	f.baseSHA, f.headSHA = makeLocalReviewGit(t, root)
	cache, err := f.manager.CachePath(901)
	if err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, root, "init", "--bare", "--template=", cache)
	if err := os.Chmod(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, filepath.Join(root, "fixture-source"), "remote", "add", "cache", cache)
	gitTestRun(t, filepath.Join(root, "fixture-source"), "push", "cache", "main", "candidate")
	f.seedConfiguration(t)
	return f
}

func (f *cliEvidenceFixture) addCredential(t *testing.T, id, principal, token string, write bool) {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	if _, err := f.store.DB().Exec(`INSERT INTO credentials(id,principal_id,token_sha256,is_admin,revoked) VALUES(?,?,?,0,0)`, id, principal, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	canWrite := 0
	if write {
		canWrite = 1
	}
	if _, err := f.store.DB().Exec(`INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES(?,?,1,?)`, id, f.project, canWrite); err != nil {
		t.Fatal(err)
	}
}

func (f *cliEvidenceFixture) seedConfiguration(t *testing.T) {
	t.Helper()
	queue := make([]int64, 0, 8)
	for number := int64(2); number <= 9; number++ {
		queue = append(queue, number)
	}
	previous := int64(0)
	configuration := reviewexec.Configuration{ExpectedRevision: &previous, CapabilityID: "runtime-capability", Mode: "independent", Queue: queue, Channels: []string{"cli", "hosted"}}
	if _, err := f.engine.Configure(context.Background(), f.scope(t, f.admin, true), "runtime-config-key", configuration); err != nil {
		t.Fatal(err)
	}
}

func (f *cliEvidenceFixture) scope(t *testing.T, token string, admin bool) core.Scope {
	return f.scopeWithAccess(t, token, admin, "write")
}

func (f *cliEvidenceFixture) scopeWithAccess(t *testing.T, token string, admin bool, access string) core.Scope {
	t.Helper()
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("project_id", f.project)
	r.Header.Set("Authorization", "Bearer "+token)
	var scope core.Scope
	var err error
	if admin {
		scope, err = f.store.AdminScope(r)
	} else {
		scope, err = f.store.Scope(r, access)
	}
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func (f *cliEvidenceFixture) startAdmitted(t *testing.T, pull int64) (reviewexec.Attempt, reviewworkspace.Workspace) {
	t.Helper()
	request := reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: pull, Channel: "cli", HeadSHA: f.headSHA}
	response, err := f.engine.Enqueue(context.Background(), f.scope(t, f.writer, false), fmt.Sprintf("request-%d", pull), request)
	if err != nil {
		t.Fatal(err)
	}
	var receipt reviewexec.Receipt
	if err := json.Unmarshal(response, &receipt); err != nil {
		t.Fatal(err)
	}
	attempt, err := f.engine.StartAttempt(context.Background(), f.project, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := f.manager.Prepare(context.Background(), 901, attempt.ID, f.headSHA, f.baseSHA)
	if err != nil {
		t.Fatalf("prepare owned workspace: %v", err)
	}
	workspace, err = f.manager.BindRepository(context.Background(), workspace, "owner/repo")
	if err != nil {
		t.Fatalf("bind canonical repository: %v", err)
	}
	anchor := reviewexec.Anchor{RepositoryID: 901, Repository: "owner/repo", PullNumber: pull, CandidateSHA: f.headSHA, BaseSHA: f.baseSHA, ParentSHA: f.baseSHA, ParentPullNumber: 0, BaseRef: "main", MergeBaseSHA: workspace.MergeBaseSHA, PatchSHA256: workspace.PatchSHA256, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	attempt, err = f.engine.Admit(context.Background(), f.project, attempt.ID, attempt.Revision, anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	return attempt, workspace
}

func makeLocalReviewGit(t *testing.T, root string) (string, string) {
	t.Helper()
	repository := filepath.Join(root, "fixture-source")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, repository, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repository, "reviewed.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, repository, "add", "reviewed.txt")
	gitTestRun(t, repository, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-m", "base")
	base := gitTestOutput(t, repository, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repository, "reviewed.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "second.txt"), []byte("added\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, repository, "add", "reviewed.txt", "second.txt")
	gitTestRun(t, repository, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-m", "candidate")
	head := gitTestOutput(t, repository, "rev-parse", "HEAD")
	gitTestRun(t, repository, "branch", "candidate")
	return base, head
}

func gitTestRun(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git fixture command failed: %v (%s)", err, output)
	}
}

func gitTestOutput(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git fixture query failed: %v", err)
	}
	return strings.TrimSpace(string(output))
}

func makeCompleteCLICapture(t *testing.T, workspace reviewworkspace.Workspace, reviewed []string) reviewprocess.Capture {
	t.Helper()
	lines := []map[string]any{
		{"type": "review_context", "reviewType": "committed", "currentBranch": "HEAD", "baseBranch": workspace.ComparisonRef, "workingDirectory": workspace.Directory},
		{"type": "finding", "title": "Synthetic finding", "comment": "A bounded synthetic observation.", "severity": "major", "fileName": "outside-diff.txt", "startLine": 1, "endLine": 2},
		{"type": "complete", "status": "review_completed", "outcome": "completed", "findings": 1, "reviewedFiles": reviewed},
	}
	var stdout strings.Builder
	for _, line := range lines {
		encoded, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		stdout.Write(encoded)
		stdout.WriteByte('\n')
	}
	return reviewprocess.Capture{Stdout: []byte(stdout.String()), ExitCode: 0, Started: true}
}

func makeIncompleteCLICapture(t *testing.T, workspace reviewworkspace.Workspace) reviewprocess.Capture {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"type": "review_context", "reviewType": "committed", "currentBranch": "HEAD", "baseBranch": workspace.ComparisonRef, "workingDirectory": workspace.Directory})
	if err != nil {
		t.Fatal(err)
	}
	return reviewprocess.Capture{Stdout: append(encoded, '\n'), ExitCode: 0, Started: true}
}

func mustChangedFiles(t *testing.T, manager *reviewworkspace.Manager, workspace reviewworkspace.Workspace) []string {
	t.Helper()
	paths, err := manager.ChangedFiles(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func assertCoreErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("expected error %q, got %v", code, err)
	}
}

func TestRetainCLIWorkspaceCaptureAndImmutableEvidence(t *testing.T) {
	f := newCLIEvidenceFixture(t)
	attempt, workspace := f.startAdmitted(t, 2)
	changed := mustChangedFiles(t, f.manager, workspace)
	if len(changed) != 2 || changed[0] != "reviewed.txt" || changed[1] != "second.txt" {
		t.Fatalf("fixture does not have the expected complete source comparison: %v", changed)
	}
	capture := makeCompleteCLICapture(t, workspace, changed)
	started, finished := time.Now().Add(-time.Second).UTC(), time.Now().UTC()
	before := cliAttemptState(t, f.store, attempt.ID)
	result, err := RetainCLI(context.Background(), f.captures, f.engine, f.manager, attempt, workspace, capture, started, finished)
	if err != nil || result.Outcome != "completed" || !result.Complete || result.Reason != "capture_retained" || result.Receipt.State != "captured" || result.Receipt.Revision != 1 {
		t.Fatalf("complete CLI capture was not retained: %+v %v", result, err)
	}
	if after := cliAttemptState(t, f.store, attempt.ID); after != before {
		t.Fatalf("capture changed attempt, operation or reservation state: before=%s after=%s", before, after)
	}
	if cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE project_id=? AND attempt_id=?`, f.project, attempt.ID) != 1 || cliCount(t, f.store, `SELECT count(*) FROM events WHERE project_id=? AND event_type='review.captured'`, f.project) != 1 {
		t.Fatal("first retained capture did not create exactly one ledger row and event")
	}
	readerScope := f.scopeWithAccess(t, f.reader, false, "read")
	stored, err := f.engine.ReadCLIEvidence(context.Background(), readerScope, result.Receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ProjectID != f.project || stored.AttemptID != attempt.ID || stored.OperationID != attempt.OperationID || stored.Anchor != *attempt.Anchor || stored.Capture.ExpectedAnchor != *attempt.Anchor || stored.Capture.Review.Outcome != "completed" || !stored.Capture.Review.Complete || stored.Capture.CaptureBytes < 1 {
		t.Fatalf("stored capture does not match the admitted evidence: %+v", stored)
	}
	if stored.Capture.CaptureID != "review_capture_"+stored.Capture.CaptureSHA256[:32] || stored.Capture.Review.ReportedFindings != 1 || len(stored.Capture.Review.ReviewedFiles) != 2 {
		t.Fatalf("capture identity or complete source list was not preserved: %+v", stored.Capture)
	}
	if stored.Capture.Review.Findings[0].Path != "outside-diff.txt" {
		t.Fatal("legitimate outside-diff finding path was lost")
	}
	operation, err := f.engine.ReadOperation(context.Background(), readerScope, attempt.OperationID)
	if err != nil || stored.CapabilityFingerprint != operation.CapabilityFingerprint {
		t.Fatalf("evidence lost its configured capability identity: %v", err)
	}
	var body string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_evidence WHERE project_id=? AND id=?`, f.project, stored.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{workspace.Directory, "review_context", string(capture.Stdout)} {
		if strings.Contains(body, private) {
			t.Fatalf("journal contains a local path or raw capture data (%q)", private)
		}
	}
	formatted := fmt.Sprintf("%+v %#v", result, result)
	if strings.Contains(formatted, workspace.Directory) || strings.Contains(formatted, string(capture.Stdout)) {
		t.Fatal("compact RetainCLI result exposed local capture details")
	}
	if runtime.GOOS != "windows" {
		artifact := filepath.Join(f.captures.root, attempt.ID+".json")
		info, err := os.Stat(artifact)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("capture artifact is unavailable or not private: %v", err)
		}
	}
	loadedCapture, captureReference, err := f.captures.Read(context.Background(), attempt.ID)
	if err != nil || captureReference.ID != stored.Capture.CaptureID || captureReference.SHA256 != stored.Capture.CaptureSHA256 || string(loadedCapture.Capture.Stdout) != string(capture.Stdout) {
		t.Fatal("private artifact does not match its journal reference", err)
	}

	// Terminal transition does not erase retained evidence; exact retry reuses
	// the original receipt and does not append a second event.
	if _, err := f.engine.Advance(context.Background(), f.project, attempt.ID, attempt.Revision, "failed", "test_finished", ""); err != nil {
		t.Fatal(err)
	}
	replayed, err := RetainCLI(context.Background(), f.captures, f.engine, f.manager, attempt, workspace, capture, started, finished)
	if err != nil || replayed.Receipt != result.Receipt || replayed.Outcome != result.Outcome || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, attempt.ID) != 1 || cliCount(t, f.store, `SELECT count(*) FROM events WHERE project_id=? AND event_type='review.captured'`, f.project) != 1 {
		t.Fatalf("exact evidence retry changed durable state: %+v %v", replayed, err)
	}
	changedCapture := capture
	changedCapture.Stderr = []byte("different capture")
	_, err = RetainCLI(context.Background(), f.captures, f.engine, f.manager, attempt, workspace, changedCapture, started, finished)
	assertCoreErrorCode(t, err, "capture_conflict")
}

func TestRetainCLIWithholdsCompleteCoverageOnCaptureOrWorkspaceMismatch(t *testing.T) {
	f := newCLIEvidenceFixture(t)
	tests := []struct {
		name    string
		prepare func(t *testing.T, attempt reviewexec.Attempt, workspace reviewworkspace.Workspace) (reviewworkspace.Workspace, reviewprocess.Capture)
		outcome string
		reason  string
	}{
		{
			name: "missing-reviewed-file",
			prepare: func(t *testing.T, _ reviewexec.Attempt, workspace reviewworkspace.Workspace) (reviewworkspace.Workspace, reviewprocess.Capture) {
				return workspace, makeCompleteCLICapture(t, workspace, []string{"reviewed.txt"})
			}, outcome: "uncertain", reason: "coverage_incomplete",
		},
		{
			name: "extra-reviewed-file",
			prepare: func(t *testing.T, _ reviewexec.Attempt, workspace reviewworkspace.Workspace) (reviewworkspace.Workspace, reviewprocess.Capture) {
				return workspace, makeCompleteCLICapture(t, workspace, []string{"reviewed.txt", "second.txt", "invented.txt"})
			}, outcome: "uncertain", reason: "coverage_incomplete",
		},
		{
			name: "malformed-structured-output",
			prepare: func(_ *testing.T, _ reviewexec.Attempt, workspace reviewworkspace.Workspace) (reviewworkspace.Workspace, reviewprocess.Capture) {
				return workspace, reviewprocess.Capture{Stdout: []byte("{\"type\":\n"), ExitCode: 0, Started: true}
			}, outcome: "uncertain", reason: "capture_invalid",
		},
		{
			name: "structured-output-without-terminal",
			prepare: func(t *testing.T, _ reviewexec.Attempt, workspace reviewworkspace.Workspace) (reviewworkspace.Workspace, reviewprocess.Capture) {
				return workspace, makeIncompleteCLICapture(t, workspace)
			}, outcome: "uncertain", reason: "capture_retained",
		},
		{
			name: "nonzero-process-exit",
			prepare: func(_ *testing.T, _ reviewexec.Attempt, workspace reviewworkspace.Workspace) (reviewworkspace.Workspace, reviewprocess.Capture) {
				return workspace, reviewprocess.Capture{Stderr: []byte("synthetic exit detail"), ExitCode: 2, Started: true}
			}, outcome: "failed", reason: "capture_retained",
		},
		{
			name: "process-not-started",
			prepare: func(_ *testing.T, _ reviewexec.Attempt, workspace reviewworkspace.Workspace) (reviewworkspace.Workspace, reviewprocess.Capture) {
				return workspace, reviewprocess.Capture{ExitCode: -1, Started: false}
			}, outcome: "failed", reason: "process_not_started",
		},
		{
			name: "workspace-source-mismatch",
			prepare: func(t *testing.T, _ reviewexec.Attempt, workspace reviewworkspace.Workspace) (reviewworkspace.Workspace, reviewprocess.Capture) {
				changed := workspace
				changed.CandidateSHA = strings.Repeat("f", 40)
				return changed, makeCompleteCLICapture(t, changed, []string{"reviewed.txt", "second.txt"})
			}, outcome: "uncertain", reason: "source_changed",
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			attempt, workspace := f.startAdmitted(t, int64(3+i))
			usedWorkspace, capture := tc.prepare(t, attempt, workspace)
			started, finished := time.Now().Add(-time.Second).UTC(), time.Now().UTC()
			before := cliAttemptState(t, f.store, attempt.ID)
			result, err := RetainCLI(context.Background(), f.captures, f.engine, f.manager, attempt, usedWorkspace, capture, started, finished)
			if err != nil || result.Outcome != tc.outcome || result.Complete || result.Reason != tc.reason {
				t.Fatalf("unexpected bounded evidence result: %+v %v", result, err)
			}
			if after := cliAttemptState(t, f.store, attempt.ID); after != before {
				t.Fatalf("uncertain capture mutated attempt state: before=%s after=%s", before, after)
			}
			stored, err := f.engine.ReadCLIEvidence(context.Background(), f.scopeWithAccess(t, f.reader, false, "read"), result.Receipt.ID)
			if err != nil || stored.Capture.Review.Outcome != tc.outcome || stored.Capture.Review.Complete || stored.Capture.CaptureBytes < 1 {
				t.Fatalf("incomplete evidence was not retained honestly: %+v %v", stored, err)
			}
			if cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, attempt.ID) != 1 || cliCount(t, f.store, `SELECT count(*) FROM events WHERE resource_id=? AND event_type='review.captured'`, result.Receipt.ID) != 1 {
				t.Fatal("retained incomplete evidence did not have exactly one ledger row and event")
			}
			if _, err := f.engine.Advance(context.Background(), f.project, attempt.ID, attempt.Revision, "failed", "test_finished", ""); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRetainCLICallerAnchorMismatchRetainsArtifactButNotLedgerCredit(t *testing.T) {
	f := newCLIEvidenceFixture(t)
	attempt, workspace := f.startAdmitted(t, 2)
	changedFiles := mustChangedFiles(t, f.manager, workspace)
	callerAttempt := attempt
	callerAnchor := *attempt.Anchor
	callerAnchor.PatchSHA256 = strings.Repeat("f", 64)
	callerAttempt.Anchor = &callerAnchor
	capture := makeCompleteCLICapture(t, workspace, changedFiles)
	started, finished := time.Now().Add(-time.Second).UTC(), time.Now().UTC()
	before := cliAttemptState(t, f.store, attempt.ID)
	_, err := RetainCLI(context.Background(), f.captures, f.engine, f.manager, callerAttempt, workspace, capture, started, finished)
	assertCoreErrorCode(t, err, "review_conflict")
	if after := cliAttemptState(t, f.store, attempt.ID); after != before {
		t.Fatalf("mismatched caller anchor mutated journal or reservation: before=%s after=%s", before, after)
	}
	if cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE project_id=? AND attempt_id=?`, f.project, attempt.ID) != 0 || cliCount(t, f.store, `SELECT count(*) FROM events WHERE project_id=? AND event_type='review.captured'`, f.project) != 0 {
		t.Fatal("mismatched caller anchor received a ledger row or capture event")
	}
	loaded, reference, err := f.captures.Read(context.Background(), attempt.ID)
	if err != nil || reference.Bytes < 1 || string(loaded.Capture.Stdout) != string(capture.Stdout) {
		t.Fatalf("raw artifact was not retained for safe diagnosis: %v", err)
	}
}

func TestRetainCLIRestartBetweenCaptureAndLedgerPreservesMeasuredTimes(t *testing.T) {
	f := newCLIEvidenceFixture(t)
	ctx := context.Background()
	attempt, workspace := f.startAdmitted(t, 2)
	record := CaptureRecord{Capture: makeCompleteCLICapture(t, workspace, mustChangedFiles(t, f.manager, workspace)), StartedAt: time.Now().Add(-2 * time.Second).UTC(), FinishedAt: time.Now().Add(-time.Second).UTC()}
	reference, err := f.captures.Save(ctx, attempt.ID, record)
	if err != nil {
		t.Fatal(err)
	}
	if cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.captured'`) != 0 {
		t.Fatal("saving an artifact unexpectedly ingested or credited evidence")
	}
	artifact := filepath.Join(f.captures.root, attempt.ID+".json")
	artifactBytes, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate losing all process-local capture state at the exact durable gap.
	// Reopen real SQLite, the capture store and the immutable workspace manager.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = core.Open(ctx, f.storeDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.store.Close() })
	f.engine, err = reviewexec.New(f.store, []reviewexec.Capability{{ID: "runtime-capability", Provider: "coderabbit", Account: "runtime-account", Channels: []string{"cli", "hosted"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.engine.RecoverInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	operation, err := f.engine.ReadOperation(ctx, f.scopeWithAccess(t, f.reader, false, "read"), attempt.OperationID)
	if err != nil || operation.State != "uncertain" {
		t.Fatal("restart did not retain possible execution as uncertain", err)
	}
	f.captures, err = NewCaptureStore(filepath.Dir(artifact))
	if err != nil {
		t.Fatal(err)
	}
	f.manager, err = reviewworkspace.New(filepath.Dir(filepath.Dir(workspace.Directory)), "git")
	if err != nil {
		t.Fatal(err)
	}
	a := *attempt.Anchor
	workspace, err = f.manager.Open(ctx, attempt.ID, a.CandidateSHA, a.BaseSHA, a.MergeBaseSHA, a.PatchSHA256, a.Repository)
	if err != nil {
		t.Fatal(err)
	}
	restored, restoredRef, err := f.captures.Read(ctx, attempt.ID)
	if err != nil || restoredRef != reference || !restored.StartedAt.Equal(record.StartedAt) || !restored.FinishedAt.Equal(record.FinishedAt) {
		t.Fatal("restart could not reconstruct original capture window", err)
	}
	before := cliAttemptState(t, f.store, attempt.ID)
	result, err := RetainCLI(ctx, f.captures, f.engine, f.manager, attempt, workspace, restored.Capture, restored.StartedAt, restored.FinishedAt)
	if err != nil || !result.Complete || result.Outcome != "completed" {
		t.Fatal("retained capture could not be ingested after restart", err)
	}
	stored, err := f.engine.ReadCLIEvidence(ctx, f.scopeWithAccess(t, f.reader, false, "read"), result.Receipt.ID)
	if err != nil || stored.Capture.StartedAt != record.StartedAt.Format(time.RFC3339Nano) || stored.Capture.FinishedAt != record.FinishedAt.Format(time.RFC3339Nano) || stored.Capture.CaptureID != reference.ID || stored.Capture.CaptureSHA256 != reference.SHA256 || stored.Capture.CaptureBytes != reference.Bytes {
		t.Fatal("ledger invented times or changed retained capture identity", err)
	}
	replay, err := RetainCLI(ctx, f.captures, f.engine, f.manager, attempt, workspace, restored.Capture, restored.StartedAt, restored.FinishedAt)
	if err != nil || replay != result || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 1 || cliCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.captured'`) != 1 || cliAttemptState(t, f.store, attempt.ID) != before {
		t.Fatal("reconstructed replay changed receipt, evidence, events or journal", err)
	}
	after, err := os.ReadFile(artifact)
	if err != nil || !bytes.Equal(artifactBytes, after) {
		t.Fatal("ledger recovery replaced original artifact", err)
	}
	_, err = RetainCLI(ctx, f.captures, f.engine, f.manager, attempt, workspace, restored.Capture, restored.StartedAt.Add(time.Nanosecond), restored.FinishedAt)
	assertCoreErrorCode(t, err, "capture_conflict")
}

func cliCount(t *testing.T, store *core.Store, query string, args ...any) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func cliAttemptState(t *testing.T, store *core.Store, attemptID string) string {
	t.Helper()
	var attemptState, anchor, operationState string
	var attemptRevision, operationRevision int64
	var reservations, transitions int
	if err := store.DB().QueryRow(`SELECT a.state,a.revision,a.anchor_json,o.state,o.revision,(SELECT count(*) FROM review_execution_reservations WHERE attempt_id=a.id),(SELECT count(*) FROM review_execution_transitions WHERE attempt_id=a.id) FROM review_execution_attempts a JOIN review_operations o ON o.id=a.operation_id WHERE a.id=?`, attemptID).Scan(&attemptState, &attemptRevision, &anchor, &operationState, &operationRevision, &reservations, &transitions); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s/%d/%s/%d/%s/reservations=%d/transitions=%d", attemptState, attemptRevision, anchor, operationRevision, operationState, reservations, transitions)
}

func TestRetainCLIExcessReviewedFilesRetainsInspectableInvalidCapture(t *testing.T) {
	f := newCLIEvidenceFixture(t)
	attempt, workspace := f.startAdmitted(t, 2)
	files := make([]string, 10001)
	for i := range files {
		files[i] = fmt.Sprintf("source-%d.go", i)
	}
	capture := makeCompleteCLICapture(t, workspace, files)
	for _, line := range strings.Split(string(capture.Stdout), "\n") {
		if len(line) >= 256<<10 {
			t.Fatal("fixture exceeds the line bound instead of the reviewed-file count")
		}
	}
	// Save exact raw evidence first, establishing the artifact that RetainCLI
	// must reconcile without rewriting when its derived parser rejects input.
	started, finished := time.Now().Add(-time.Second).UTC(), time.Now().UTC()
	reference, err := f.captures.Save(t.Context(), attempt.ID, CaptureRecord{Capture: capture, StartedAt: started, FinishedAt: finished})
	if err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(f.captures.root, attempt.ID+".json")
	beforeArtifact, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	beforeState := cliAttemptState(t, f.store, attempt.ID)
	retained, err := RetainCLI(t.Context(), f.captures, f.engine, f.manager, attempt, workspace, capture, started, finished)
	if err != nil || retained.Reason != "capture_invalid" || retained.Outcome != "uncertain" || retained.Complete || retained.Receipt.State != "captured" {
		t.Fatalf("oversized inventory lost inspectable invalid evidence: %+v %v", retained, err)
	}
	stored, err := f.engine.ReadCLIEvidence(t.Context(), f.scopeWithAccess(t, f.reader, false, "read"), retained.Receipt.ID)
	if err != nil || stored.Capture.Review.Complete || stored.Capture.Review.Outcome != "uncertain" || len(stored.Capture.Review.ReviewedFiles) != 0 || len(stored.Capture.Review.Findings) != 0 || stored.Capture.Review.ReportedFindings != 0 {
		t.Fatalf("invalid inventory escaped derived ledger bounds or acquired completed coverage: %+v %v", stored, err)
	}
	if stored.Capture.CaptureID != reference.ID || stored.Capture.CaptureSHA256 != reference.SHA256 || stored.Capture.CaptureBytes != reference.Bytes {
		t.Fatal("invalid ledger evidence lost its exact raw artifact identity")
	}
	loaded, afterReference, err := f.captures.Read(t.Context(), attempt.ID)
	if err != nil || afterReference != reference || string(loaded.Capture.Stdout) != string(capture.Stdout) || string(loaded.Capture.Stderr) != string(capture.Stderr) || loaded.Capture.ExitCode != capture.ExitCode || loaded.Capture.Started != capture.Started {
		t.Fatal("raw oversized capture was changed or lost", err)
	}
	afterArtifact, err := os.ReadFile(artifact)
	if err != nil || string(beforeArtifact) != string(afterArtifact) {
		t.Fatal("retention rewrote the immutable raw capture", err)
	}
	if cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE project_id=? AND attempt_id=?`, f.project, attempt.ID) != 1 || cliCount(t, f.store, `SELECT count(*) FROM events WHERE resource_id=? AND event_type='review.captured'`, retained.Receipt.ID) != 1 || cliAttemptState(t, f.store, attempt.ID) != beforeState {
		t.Fatal("invalid retention lost its single ledger/event or changed attempt state")
	}
}
