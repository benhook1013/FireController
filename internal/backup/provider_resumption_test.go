package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
)

func resumptionFixture(t *testing.T) (*activationFixture, ProviderResumptionRequest, ActivationSummary) {
	t.Helper()
	f := activationTestFixture(t)
	activation, err := Activate(t.Context(), f.dir, f.request)
	if err != nil {
		t.Fatal(err)
	}
	return f, ProviderResumptionRequest{ExpectedInstanceID: activation.InstanceID, FencedPredecessorID: activation.SourceInstanceID, ProviderEffectsSettled: true, AbandonRetainedWork: true, Reason: "External effects independently settled", EvidenceReference: "operator-owned://synthetic-proof"}, activation
}

func TestProviderResumptionReconcilesAtomicallyAndExactReplayIsHistorical(t *testing.T) {
	f, in, activation := resumptionFixture(t)
	before := activationRows(t, f.dir)
	released, err := ResumeProviders(t.Context(), f.dir, in)
	if err != nil || released.InstanceID != activation.InstanceID || released.SourceInstanceID != activation.SourceInstanceID || released.BundleID != activation.BundleID || released.State != "coordination_active" || released.ProvidersHeld || released.ReleasedAt == "" || released.Counts.Attempts != 1 || released.Counts.PausedProjects != 1 {
		t.Fatal("provider release failed", released, err)
	}
	after := activationRows(t, f.dir)
	for _, table := range []string{"credentials", "grants", "receipts", "jobs", "job_history", "inbox_messages", "inbox_recipient_state", "review_hosted_allowance_queries"} {
		original, present := before[table]
		retained, stillPresent := after[table]
		if !present || !stillPresent || !reflect.DeepEqual(original, retained) {
			t.Fatal("release rewrote unrelated retained state", table)
		}
	}
	// Revocation and new pending work after release cannot be reversed by retry.
	s := activationOpen(t, f.dir)
	e, err := reviewexec.New(s, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("project_id", f.source.project)
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("X-FC-Instance", s.InstanceID())
	r.Header.Set("X-FC-Worker", f.source.worker)
	scope, err := s.AdminScope(r)
	if err != nil {
		t.Fatal(err)
	}
	previous := int64(2)
	if _, err = e.Configure(t.Context(), scope, "post-release-config", reviewexec.Configuration{ExpectedRevision: &previous, CapabilityID: "restore-cap", Mode: "independent", Queue: []int64{2, 3}, Channels: []string{"hosted"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Enqueue(t.Context(), scope, "post-release-work", reviewexec.Request{ExpectedConfigurationRevision: 3, PullNumber: 3, Channel: "hosted", HeadSHA: strings.Repeat("a", 40)}); err != nil {
		t.Fatal(err)
	}
	activationExec(t, f.dir, `UPDATE credentials SET revoked=1 WHERE id=?`, activation.CredentialID)
	beforeReplay := activationRows(t, f.dir)
	replayed, err := ResumeProviders(t.Context(), f.dir, in)
	if err != nil || replayed != released || !reflect.DeepEqual(beforeReplay, activationRows(t, f.dir)) {
		t.Fatal("exact retry was not historical/read-only", replayed, err)
	}
	oldActivation, err := Activate(t.Context(), f.dir, f.request)
	if err != nil || oldActivation != activation || !oldActivation.ProvidersHeld {
		t.Fatal("activation retry changed its original held result", oldActivation, err)
	}
	status, err := ReadRecoveryStatus(t.Context(), s)
	if err != nil || status.ProvidersHeld || StartupGuard(t.Context(), s) != nil {
		t.Fatal("released coordination status was not retained", status, err)
	}
	changed := in
	changed.Reason += " changed"
	if _, err = ResumeProviders(t.Context(), f.dir, changed); err == nil || !reflect.DeepEqual(beforeReplay, activationRows(t, f.dir)) {
		t.Fatal("changed release request mutated history", err)
	}
}

func TestProviderResumptionRefusalsAndEscapedAuditRollback(t *testing.T) {
	for _, mode := range []string{"identity", "predecessor", "effects", "abandon", "empty", "control", "invalid-utf8", "too-long", "escaped-audit"} {
		t.Run(mode, func(t *testing.T) {
			f, in, _ := resumptionFixture(t)
			before := activationRows(t, f.dir)
			switch mode {
			case "identity":
				in.ExpectedInstanceID = core.NewID("instance")
			case "predecessor":
				in.FencedPredecessorID = core.NewID("instance")
			case "effects":
				in.ProviderEffectsSettled = false
			case "abandon":
				in.AbandonRetainedWork = false
			case "empty":
				in.Reason = " "
			case "control":
				in.EvidenceReference = "unsafe\nreference"
			case "invalid-utf8":
				in.Reason = string([]byte{255})
			case "too-long":
				in.Reason = strings.Repeat("x", 257)
			case "escaped-audit":
				in.Reason, in.EvidenceReference = strings.Repeat("<", 256), strings.Repeat("&", 256)
			}
			_, err := ResumeProviders(t.Context(), f.dir, in)
			if err == nil || !reflect.DeepEqual(before, activationRows(t, f.dir)) {
				t.Fatal("refused release changed retained work/access/history", err)
			}
			if mode == "escaped-audit" {
				var failure *core.Error
				if !errors.As(err, &failure) || failure.Code != "restore_release_too_large" {
					t.Fatal("escaped audit did not roll back after reconciliation", err)
				}
			}
		})
	}
}

func TestProviderResumptionConcurrentExactRetryAndFutureActivation(t *testing.T) {
	f, in, _ := resumptionFixture(t)
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	activationExec(t, f.dir, `UPDATE metadata SET value=? WHERE key='restore:activated_at'`, future)
	var results [2]ProviderResumptionSummary
	var errs [2]error
	var group sync.WaitGroup
	for i := range 2 {
		group.Go(func() { results[i], errs[i] = ResumeProviders(t.Context(), f.dir, in) })
	}
	group.Wait()
	if errs[0] != nil || errs[1] != nil || results[0] != results[1] || results[0].ReleasedAt != future || results[0].Counts.Attempts != 1 {
		t.Fatal("concurrent exact retry/clock floor lost original release", results, errs)
	}
}

func TestProviderResumptionReleasedGenerationRestoresHeld(t *testing.T) {
	f, in, _ := resumptionFixture(t)
	if _, err := ResumeProviders(t.Context(), f.dir, in); err != nil {
		t.Fatal(err)
	}
	original := generationBody(t, f.dir)
	bundle, err := CreateFromDirectory(t.Context(), f.dir, filepath.Join(t.TempDir(), "bundles"))
	if err != nil {
		t.Fatal("released source backup", err)
	}
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("released provenance inspection", err)
	}
	dir := filepath.Join(t.TempDir(), "next-generation")
	restored, err := Restore(t.Context(), bundle.Path, dir)
	if err != nil {
		t.Fatal("released generation restore", err)
	}
	_, file := generationToken(t)
	active, err := Activate(t.Context(), dir, ActivationRequest{ExpectedInstanceID: restored.InstanceID, FencedPredecessorID: restored.SourceInstanceID, AdminTokenFile: file})
	if err != nil || !active.ProvidersHeld {
		t.Fatal("old release granted new generation provider access", active, err)
	}
	rows := generationRead(t, dir, `SELECT body_json FROM restore_history`)
	if len(rows) != 1 || rows[0][0] != original || generationBody(t, f.dir) != original {
		t.Fatal("released provenance was rewritten instead of archived")
	}
	var fields map[string]string
	if json.Unmarshal([]byte(generationBody(t, dir)), &fields) != nil || len(fields) != 9 || fields["restore:provider_release"] != "" {
		t.Fatal("new activation inherited predecessor release")
	}
}

func TestProviderResumptionRejectsCorruptAndOversizedMetadata(t *testing.T) {
	for _, mode := range []string{"unknown", "oversized-key", "oversized-value", "invalid-count", "missing-count", "unknown-release", "wrong-identity", "early-time"} {
		t.Run(mode, func(t *testing.T) {
			f, in, _ := resumptionFixture(t)
			if _, err := ResumeProviders(t.Context(), f.dir, in); err != nil {
				t.Fatal(err)
			}
			if mode == "unknown" || mode == "oversized-key" || mode == "oversized-value" {
				key, value := "restore:extra", "extra"
				if mode == "oversized-key" {
					key += strings.Repeat("x", 129)
				}
				if mode == "oversized-value" {
					value = strings.Repeat("x", 4097)
				}
				activationExec(t, f.dir, `INSERT INTO metadata VALUES(?,?)`, key, value)
			} else {
				rows := generationRead(t, f.dir, `SELECT value FROM metadata WHERE key='restore:provider_release'`)
				var value providerRelease
				if json.Unmarshal([]byte(rows[0][0].(string)), &value) != nil {
					t.Fatal("fixture release")
				}
				switch mode {
				case "invalid-count":
					value.Counts.Attempts = -1
				case "missing-count":
				case "unknown-release":
				case "wrong-identity":
					value.Request.ExpectedInstanceID = core.NewID("instance")
				case "early-time":
					value.ReleasedAt = "2000-01-01T00:00:00Z"
				}
				body, _ := json.Marshal(value)
				if mode == "missing-count" {
					body = []byte(strings.Replace(string(body), `"attempts":1,`, "", 1))
				}
				if mode == "unknown-release" {
					body = append(body[:len(body)-1], []byte(`,"extra":false}`)...)
				}
				activationExec(t, f.dir, `UPDATE metadata SET value=? WHERE key='restore:provider_release'`, string(body))
			}
			before := activationRows(t, f.dir)
			if _, err := ResumeProviders(t.Context(), f.dir, in); err == nil || !reflect.DeepEqual(before, activationRows(t, f.dir)) {
				t.Fatal("corrupt release accepted or repaired", err)
			}
		})
	}
}

func TestProviderResumptionHeldWriterHonorsCallerDeadline(t *testing.T) {
	f, in, _ := resumptionFixture(t)
	db, err := sql.Open("sqlite", filepath.Join(f.dir, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`ROLLBACK`)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err = ResumeProviders(ctx, f.dir, in); err == nil || time.Since(started) > 300*time.Millisecond {
		t.Fatal("local lock ignored smaller caller deadline", err, time.Since(started))
	}
}

func TestProviderResumptionHeldWriterHonorsEarlyCancellation(t *testing.T) {
	f, in, _ := resumptionFixture(t)
	db, err := sql.Open("sqlite", filepath.Join(f.dir, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`ROLLBACK`)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := ResumeProviders(ctx, f.dir, in); done <- err }()
	timer := time.AfterFunc(10*time.Millisecond, cancel)
	defer timer.Stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled release succeeded")
		}
	case <-time.After(300 * time.Millisecond):
		t.Error("cancellation did not interrupt local lock wait")
		_, _ = db.Exec(`ROLLBACK`)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("release did not stop after lock cleanup")
		}
	}
}

func TestProviderResumptionOrdinaryAndInactiveRemainUnchanged(t *testing.T) {
	f := activationTestFixture(t)
	for _, dir := range []string{f.dir, f.source.dir} {
		before := activationRows(t, dir)
		in := ProviderResumptionRequest{ExpectedInstanceID: f.request.ExpectedInstanceID, FencedPredecessorID: f.request.FencedPredecessorID, ProviderEffectsSettled: true, AbandonRetainedWork: true, Reason: "settled", EvidenceReference: "private-proof"}
		if dir == f.source.dir {
			in.ExpectedInstanceID = f.source.store.InstanceID()
			in.FencedPredecessorID = core.NewID("instance")
		}
		if _, err := ResumeProviders(t.Context(), dir, in); err == nil || !reflect.DeepEqual(before, activationRows(t, dir)) {
			t.Fatal("ordinary/inactive source changed", err)
		}
	}
}

func TestProviderResumptionReleasedGenerationDoesNotAdoptLegacySchedule(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	managedRecoveryAttempt(t, m)
	past := time.Now().UTC().Add(-time.Hour)
	if err := m.update(t.Context(), func(r *managedRecord, _ *sql.Tx) error {
		r.SourceInstanceID = ""
		r.Status.LastAttempt.SourceInstanceID = ""
		r.Status.LastAttempt.StartedAt = managedStamp(past.Add(-time.Minute))
		r.Status.LastAttempt.FinishedAt = managedStamp(past)
		r.Status.NextAttemptAt = managedStamp(past)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s, dir := managedActivatedCopy(t, f)
	status, err := ReadRecoveryStatus(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	in := ProviderResumptionRequest{ExpectedInstanceID: status.InstanceID, FencedPredecessorID: status.SourceInstanceID, ProviderEffectsSettled: true, AbandonRetainedWork: true, Reason: "settled", EvidenceReference: "private-proof"}
	if _, err = ResumeProviders(t.Context(), dir, in); err != nil {
		t.Fatal(err)
	}
	next := managedFreshDue(t, s, dir, m.config)
	before := next.next
	// Once this generation has explicitly selected its own schedule, restarts
	// preserve that due time even though its provider hold has been released.
	restarted, err := NewManager(s, dir, m.config)
	if err != nil || !restarted.next.Equal(before) {
		t.Fatal("same-generation schedule was lost", err)
	}
}
