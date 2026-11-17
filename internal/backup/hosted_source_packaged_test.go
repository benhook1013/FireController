//go:build packaged_smoke

package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviewruntime"
)

// Following tools/packagedsmoke's supplied-binary/private-fixture pattern, trusted
// setup calls the real retainer before starting fcd. This proves packaged
// retention/read/restart/backup, not provider collection by the daemon.
func TestPackagedHostedSubmittedSourceRetention(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owned process stop uses POSIX interrupt; Linux packaged boundary")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	bin := os.Getenv("FC_SMOKE_BIN_DIR")
	if !filepath.IsAbs(bin) {
		t.Fatal("FC_SMOKE_BIN_DIR must select prebuilt absolute inputs")
	}
	fcd, fc := filepath.Join(bin, "fcd"), filepath.Join(bin, "fc")
	hash := func(p string) [32]byte {
		t.Helper()
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal("packaged input unavailable")
		}
		return sha256.Sum256(b)
	}
	hashes := [][32]byte{hash(fcd), hash(fc)}
	f, evidence, capturePath := submittedReviewInventoryFixture(t)
	original, err := os.ReadFile(filepath.Join(f.dir, capturePath))
	if err != nil {
		t.Fatal(err)
	}
	var retainedBody, retainedFingerprint string
	if err := f.store.DB().QueryRow(`SELECT body_json,fingerprint FROM review_execution_evidence WHERE id=?`, evidence.ID).Scan(&retainedBody, &retainedFingerprint); err != nil {
		t.Fatal(err)
	}
	later := packagedCompletionEvidence(t, f, evidence, original)
	var originalAdjudication string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_adjudication_state WHERE evidence_id=?`, evidence.ID).Scan(&originalAdjudication); err != nil {
		t.Fatal(err)
	}
	instance := f.store.InstanceID()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	private, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var external atomic.Int64
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(502) }))
	defer deny.Close()
	env := []string{"HOME=" + private, "USERPROFILE=" + private, "TMPDIR=" + private, "TMP=" + private, "TEMP=" + private, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
	credential := filepath.Join(private, "credential")
	if err := os.WriteFile(credential, []byte(f.token), 0600); err != nil {
		t.Fatal(err)
	}
	var originalRead []byte
	for pass := 0; pass < 2; pass++ {
		command := exec.CommandContext(ctx, fcd, "--data-dir", f.dir, "--listen", "127.0.0.1:0")
		command.Env = env
		command.WaitDelay = time.Second
		stdout, err := command.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		command.Stderr = &stderr
		if err := command.Start(); err != nil {
			t.Fatal("packaged daemon failed to start")
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		var once sync.Once
		stop := func() {
			once.Do(func() {
				_ = command.Process.Signal(os.Interrupt)
				select {
				case err := <-done:
					if err != nil {
						t.Error("packaged daemon stop failed")
					}
				case <-time.After(5 * time.Second):
					_ = command.Process.Kill()
					<-done
					t.Error("packaged daemon stop exceeded bound")
				}
			})
		}
		t.Cleanup(stop)
		type listening struct {
			Event      string `json:"event"`
			Address    string `json:"address"`
			InstanceID string `json:"instance_id"`
		}
		ready := make(chan listening, 1)
		go func() { var v listening; _ = json.NewDecoder(io.LimitReader(stdout, 64<<10)).Decode(&v); ready <- v }()
		var record listening
		select {
		case record = <-ready:
		case <-time.After(5 * time.Second):
			stop()
			t.Fatal("packaged daemon readiness exceeded bound")
		}
		host, _, err := net.SplitHostPort(record.Address)
		if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || record.Event != "listening" || record.InstanceID != instance {
			stop()
			t.Fatal("invalid packaged listening identity")
		}
		profiles := filepath.Join(private, "profiles.json")
		profileBody, _ := json.Marshal(map[string]any{"version": 1, "profiles": map[string]any{"source": map[string]any{"server_url": "http://" + record.Address, "instance_id": instance, "project_id": f.project, "worker_id": f.worker, "credential_file": credential}}})
		if err := os.WriteFile(profiles, profileBody, 0600); err != nil {
			t.Fatal(err)
		}
		readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
		call := exec.CommandContext(readCtx, fc, "--profiles-file", profiles, "--profile", "source", "reviews", "evidence", evidence.ID)
		call.Env = env
		output, err := call.Output()
		readCancel()
		if err != nil {
			stop()
			t.Fatal("actual CLI submitted evidence read failed")
		}
		var got client.ReviewEvidence
		if json.Unmarshal(output, &got) != nil || got.Capture.Hosted == nil || got.Capture.Hosted.SubmittedReview == nil || got.Capture.Hosted.SubmittedReview.ProviderRunID != evidence.Capture.Hosted.SubmittedReview.ProviderRunID || got.Capture.Hosted.SubmittedReview.AcknowledgementCommentID != 400 || got.Capture.Hosted.PublicationID != "" || got.Capture.Hosted.ProviderAttemptID != "" || !reflect.DeepEqual(got.Capture.Hosted.ReviewIDs, evidence.Capture.Hosted.ReviewIDs) || len(got.Capture.Findings) != 1 || got.Capture.Findings[0].Detail != evidence.Capture.Review.Findings[0].Detail {
			stop()
			t.Fatal("packaged evidence lost real identities or original finding")
		}
		if pass == 0 {
			originalRead = append([]byte(nil), output...)
		} else if !bytes.Equal(originalRead, output) {
			stop()
			t.Fatal("restart changed exact evidence response")
		}
		// The selected evidence has a genuinely different original anchor. A prior
		// finalized review still supplies channel completion, while this unfinalized
		// later capture remains pending across the whole PR/channel.
		assess := exec.CommandContext(ctx, fc, "--profiles-file", profiles, "--profile", "source", "reviews", "assess", later.ID)
		assess.Env = env
		assessed, err := assess.Output()
		var policy client.ReviewPolicyAssessment
		if err != nil || json.Unmarshal(assessed, &policy) != nil || policy.State != "held" || policy.Anchor.CandidateSHA != later.Anchor.CandidateSHA || policy.Anchor.CandidateSHA == evidence.Anchor.CandidateSHA || len(policy.Channels) != 1 || !policy.Channels[0].Covered || !policy.Channels[0].Pending {
			stop()
			t.Fatal("packaged movement lost completed channel or pending original work", err)
		}
		stop()
	}
	local := func(args ...string) []byte {
		t.Helper()
		bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		command := exec.CommandContext(bounded, fcd, args...)
		command.Env = env
		output, err := command.Output()
		if err != nil {
			t.Fatal("packaged local backup operation failed")
		}
		return output
	}
	var bundle Summary
	if json.Unmarshal(local("backup", "--data-dir", f.dir, "--destination", filepath.Join(private, "bundles")), &bundle) != nil || bundle.State != "complete" {
		t.Fatal("packaged backup failed")
	}
	var inspected Summary
	if json.Unmarshal(local("backup-inspect", "--bundle", bundle.Path), &inspected) != nil || inspected.State != "verified" {
		t.Fatal("packaged inspect failed")
	}
	destination := filepath.Join(private, "restored")
	var restored RestoreSummary
	if json.Unmarshal(local("restore", "--bundle", bundle.Path, "--data-dir", destination), &restored) != nil || restored.State != "inactive" || restored.InstanceID == instance {
		t.Fatal("packaged inactive restore failed")
	}
	for _, dir := range []string{f.dir, destination} {
		raw, err := os.ReadFile(filepath.Join(dir, capturePath))
		if err != nil || !bytes.Equal(raw, original) {
			t.Fatal("packaged operations changed original capture bytes")
		}
		store, err := core.Open(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		var body, fingerprint string
		err = store.DB().QueryRow(`SELECT body_json,fingerprint FROM review_execution_evidence WHERE id=?`, evidence.ID).Scan(&body, &fingerprint)
		if err != nil || body != retainedBody || fingerprint != retainedFingerprint {
			store.Close()
			t.Fatal("packaged operations changed immutable ledger")
		}
		var restoredAdjudication string
		if err := store.DB().QueryRow(`SELECT body_json FROM review_adjudication_state WHERE evidence_id=?`, evidence.ID).Scan(&restoredAdjudication); err != nil || restoredAdjudication != originalAdjudication {
			store.Close()
			t.Fatal("packaged restore changed original frozen counts or decisions", err)
		}
		if err := reviewexec.ValidateComparisonProofs(ctx, store.DB()); err != nil {
			store.Close()
			t.Fatal("packaged restore lost derived authority", err)
		}
		var ready, pending int
		if err := store.DB().QueryRow(`SELECT sum(state='ready'),sum(state='pending') FROM review_comparison_proofs WHERE project_id=? AND pull_number=2 AND channel='hosted'`, f.project).Scan(&ready, &pending); err != nil || ready != 1 || pending != 1 {
			store.Close()
			t.Fatal("restore changed channel completion/pending proof", err)
		}
		if dir == destination {
			backupCode(t, StartupGuard(ctx, store), "restore_inactive")
			var active int
			if store.DB().QueryRow(`SELECT count(*) FROM credentials WHERE revoked<>1`).Scan(&active) != nil || active != 0 {
				store.Close()
				t.Fatal("restore revived old credentials")
			}
		}
		_ = store.Close()
	}
	if external.Load() != 0 || hash(fcd) != hashes[0] || hash(fc) != hashes[1] {
		t.Fatal("packaged proof contacted provider or changed supplied binaries")
	}
	t.Logf("actual fcd/fc submitted retention, restart, backup/inspect/inactive restore passed; fcd=%x fc=%x external_http=0", hashes[0], hashes[1])
}

// Use the real retainer and native APIs for both heads. No cached proof is seeded.
func packagedCompletionEvidence(t *testing.T, f *restoreFixture, original reviewexec.ReviewEvidence, raw []byte) reviewexec.ReviewEvidence {
	t.Helper()
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	scope := routePolicyInventoryScope(t, f)
	revision := int64(0)
	if _, err := e.DecideReviewFinding(t.Context(), scope, "packaged-original-reject", original.ID, reviewexec.FindingDecisionRequest{ExpectedRevision: &revision, Finding: 0, Disposition: "rejected", Reason: "Explicit synthetic false-positive decision"}); err != nil {
		t.Fatal(err)
	}
	revision = 1
	if _, err := e.FinalizeReviewEvidence(t.Context(), scope, "packaged-original-finalize", original.ID, reviewexec.FinalizeReviewRequest{ExpectedRevision: &revision}); err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("9", 40)
	body, err := e.Enqueue(t.Context(), scope, "packaged-later-head", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "hosted", HeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	var receipt reviewexec.Receipt
	if json.Unmarshal(body, &receipt) != nil {
		t.Fatal("later receipt")
	}
	a, err := e.StartAttempt(t.Context(), f.project, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	anchor := original.Anchor
	anchor.CandidateSHA = head
	anchor.BaseSHA = strings.Repeat("8", 40)
	anchor.ParentSHA = anchor.BaseSHA
	anchor.MergeBaseSHA = anchor.BaseSHA
	anchor.PatchSHA256 = strings.Repeat("7", 64)
	anchor.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	query, created, err := e.BeginHostedAllowanceQuery(t.Context(), f.project, a.ID, a.Revision, anchor, reviewprovider.Author{ID: 1, Login: "fixture-user", Kind: "User"}, reviewprovider.Author{ID: 2, Login: "fixture-bot", Kind: "Bot"})
	if err != nil || !created {
		t.Fatal("later native allowance intent", err)
	}
	intent := reviewprovider.HostedTriggerIntent{OperationID: query.OperationID, AttemptID: a.ID, HeadSHA: head}
	command, err := reviewprovider.HostedTriggerBody(intent)
	if err != nil {
		t.Fatal(err)
	}
	a, err = e.Admit(t.Context(), f.project, a.ID, a.Revision, anchor, command)
	if err != nil {
		t.Fatal(err)
	}
	a, err = e.Advance(t.Context(), f.project, a.ID, a.Revision, "active", "synthetic_posted", "1100")
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := e.ReadHostedRecovery(t.Context(), f.project, a.ID, "restore-cap")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Record reviewruntime.HostedCaptureRecord `json:"record"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		t.Fatal("original raw fixture")
	}
	record := envelope.Record
	record.Intent = intent
	record.EarliestPosting = recovery.EarliestPosting
	at := time.Now().UTC()
	record.StartedAt = at.Add(400 * time.Millisecond)
	record.FinishedAt = at.Add(600 * time.Millisecond)
	record.Snapshot.ObservedAt = at.Add(500 * time.Millisecond)
	for i := range record.Snapshot.IssueComments {
		c := &record.Snapshot.IssueComments[i]
		c.ID += 1000
		c.CreatedAt = at
		c.UpdatedAt = at
		if i == 0 {
			c.Body = command
		} else {
			c.CreatedAt = at.Add(300 * time.Millisecond)
			c.UpdatedAt = c.CreatedAt
		}
	}
	for i := range record.Snapshot.Reviews {
		r := &record.Snapshot.Reviews[i]
		r.ID += 1000
		r.HeadSHA = head
		r.SubmittedAt = at.Add(200 * time.Millisecond)
		r.Body = strings.ReplaceAll(r.Body, original.Anchor.CandidateSHA, head)
		r.Body = strings.ReplaceAll(r.Body, original.Anchor.ParentSHA, anchor.ParentSHA)
		r.Body = strings.ReplaceAll(r.Body, "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444")
	}
	for i := range record.Snapshot.ReviewComments {
		c := &record.Snapshot.ReviewComments[i]
		c.ID += 1000
		c.ReviewID += 1000
		c.HeadSHA = head
		c.OriginalHeadSHA = head
		c.CreatedAt = at.Add(100 * time.Millisecond)
		c.UpdatedAt = c.CreatedAt
	}
	captures, err := reviewruntime.NewHostedCaptureStore(filepath.Join(f.dir, "hosted-review-captures"))
	if err != nil {
		t.Fatal(err)
	}
	retained, err := reviewruntime.RetainHosted(t.Context(), captures, e, a, record)
	if err != nil || !retained.Complete {
		t.Fatal("later genuine retained capture", retained, err)
	}
	if _, err := e.Advance(t.Context(), f.project, a.ID, a.Revision, "completed", "synthetic_captured", "1100"); err != nil {
		t.Fatal(err)
	}
	later, err := e.ReadReviewEvidence(t.Context(), scope, retained.Receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := e.ReadReviewAdjudication(t.Context(), scope, original.ID)
	if err != nil || state.FrozenCounts == nil || *state.FrozenCounts != (reviewexec.FindingCounts{Found: 1}) || later.Anchor == original.Anchor {
		t.Fatal("original frozen authority changed", err)
	}
	return later
}
