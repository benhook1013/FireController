package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func configureLifecycleTwoPulls(t *testing.T, f *attemptFixture) {
	t.Helper()
	previous := int64(2)
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "storage-two-pulls", Configuration{ExpectedRevision: &previous, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}, Paused: true}); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleStoredMetadataIsolationAndRecovery(t *testing.T) {
	for _, lane := range []lifecycleLane{lifecycleMetadata, lifecycleEnrichment} {
		t.Run(map[lifecycleLane]string{lifecycleMetadata: "metadata", lifecycleEnrichment: "enrichment"}[lane], func(t *testing.T) {
			f, s, clock := lifecycleFixture(t)
			configureLifecycleTwoPulls(t, f)
			f.e.lifecycleSchedule = s
			f.e.lifecycleRunning.Store(true)
			admissionSQL(t, f, `UPDATE review_pull_versions SET metadata_json='{"is_draft":"bad"}' WHERE project_id=? AND number=2`, attemptProject)
			r := externalReader(externalSnapshot(0, strings.Repeat("a", 40)))
			r.pull = func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
				p := lifecyclePull(n)
				p.Title = "independent metadata"
				return p, nil
			}
			readers := map[string]TargetReader{"test-capability": r}
			// First selection holds malformed PR 2 before any provider read.
			if target, err := f.e.claimLifecycleLane(t.Context(), readers, s, lane); err != nil || target != nil {
				t.Fatal(target, err)
			}
			if r.calls.Load() != 0 {
				t.Fatal("malformed retained target reached provider")
			}
			target, err := f.e.claimLifecycleLane(t.Context(), readers, s, lane)
			if err != nil || target == nil || target.number != 4 {
				t.Fatal("held PR starved unrelated target", target, err)
			}
			if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil {
				t.Fatal(err)
			}
			var got reviews.PullResponse
			projectionRead(t, f, "/pulls/4", &got)
			if got.Pull.Title != "independent metadata" {
				t.Fatal("valid independent metadata did not publish", got.Pull)
			}
			_, health, err := f.e.ObservationHealth(t.Context(), attemptProject, []reviews.PullSummary{{Number: 2}, {Number: 4}})
			if err != nil || health[2].Error != "stored_metadata_invalid" || health[2].State != "retrying" {
				t.Fatal(health, err)
			}
			if account := s.accounts[target.account]; !account.until.IsZero() || account.lastErrorCode != "" {
				t.Fatal("local corruption poisoned provider account", account)
			}
			if target, err := f.e.claimLifecycleLane(t.Context(), readers, s, lane); err != nil || target != nil {
				t.Fatal("malformed PR busy-looped", target, err)
			}
			admissionSQL(t, f, `UPDATE review_pull_versions SET metadata_json='{}' WHERE project_id=? AND number=2`, attemptProject)
			clock.Add(time.Minute)
			target, err = f.e.claimLifecycleLane(t.Context(), readers, s, lane)
			if err != nil || target == nil || target.number != 2 {
				t.Fatal("repaired PR did not recover", target, err)
			}
			if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil {
				t.Fatal(err)
			}
			d := s.due[lifecycleKey{attemptProject, 2}]
			if d.lastErrorCode != "" || d.enrichmentError != "" {
				t.Fatal("repaired target retained error", d)
			}
		})
	}
}

func TestLifecycleInvalidConfigurationHoldsOnlyProject(t *testing.T) {
	for _, body := range []any{`{"queue":"bad"}`, `{"unexpected":true}`, []byte(`{}`)} {
		t.Run(stringifyLifecycleBody(body), func(t *testing.T) {
			f, s, _ := lifecycleFixture(t)
			f.configure(t, otherAttemptProject, "test-capability", 0, true)
			request := reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: 802, Source: "imported-history", ObservedAt: "2026-01-01T00:00:00Z", Queue: []reviews.PullInput{projectionPull(2)}}
			w := f.request("POST", "/api/v1/projects/"+otherAttemptProject+"/reviews/observations", f.admin, "independent-project", encoded(request))
			if w.Code != 201 {
				t.Fatal(w.Code, w.Body.String())
			}
			admissionSQL(t, f, `UPDATE review_execution_config SET body_json=? WHERE project_id=?`, body, attemptProject)
			r := &refreshTestReader{repository: func(_ context.Context, id int64) (reviewprovider.Repository, error) {
				return reviewprovider.Repository{ID: id, FullName: "owner/repo"}, nil
			}, pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
				p := lifecyclePull(n)
				p.RepositoryID = 802
				p.Title = "other project advanced"
				return p, nil
			}}
			target, err := f.e.claimLifecycle(t.Context(), map[string]TargetReader{"test-capability": r}, s)
			if err != nil || target == nil || target.project != otherAttemptProject {
				t.Fatal("configuration corruption stopped independent project", target, err)
			}
			if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil {
				t.Fatal(err)
			}
			var title string
			if err = f.store.DB().QueryRow(`SELECT v.title FROM review_current_pulls c JOIN review_pull_memberships m ON m.project_id=c.project_id AND m.number=c.number AND m.start_revision=c.start_revision JOIN review_pull_versions v ON v.project_id=m.project_id AND v.number=m.number AND v.version_revision=m.version_revision WHERE c.project_id=? AND c.number=2`, otherAttemptProject).Scan(&title); err != nil || title != "other project advanced" {
				t.Fatal(title, err)
			}
			observer, health, err := f.e.ObservationHealth(t.Context(), attemptProject, []reviews.PullSummary{{Number: 2}})
			if err != nil || observer.State != "unavailable" || health[2].Error != "stored_configuration_invalid" {
				t.Fatal(observer, health, err)
			}
		})
	}
}
func stringifyLifecycleBody(body any) string {
	if _, ok := body.([]byte); ok {
		return "blob"
	}
	return body.(string)
}

func TestLifecycleCorruptionDuringProviderReadRollsBackAndHolds(t *testing.T) {
	for _, mode := range []string{"configuration", "metadata"} {
		t.Run(mode, func(t *testing.T) {
			f, s, _ := lifecycleFixture(t)
			r := &refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
				if mode == "configuration" {
					admissionSQL(t, f, `UPDATE review_execution_config SET body_json='{}' WHERE project_id=?`, attemptProject)
				} else {
					admissionSQL(t, f, `UPDATE review_pull_versions SET metadata_json=? WHERE project_id=? AND number=2`, []byte(`{}`), attemptProject)
				}
				p := lifecyclePull(n)
				p.Title = "must roll back"
				return p, nil
			}}
			target, err := f.e.claimLifecycle(t.Context(), map[string]TargetReader{"test-capability": r}, s)
			if err != nil || target == nil {
				t.Fatal(target, err)
			}
			before := attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`)
			events := attemptCount(t, f.store, `SELECT count(*) FROM events`)
			if err = f.e.observeLifecycle(t.Context(), *target, s); err != nil {
				t.Fatal("scoped invalid state escaped", err)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`) != before || attemptCount(t, f.store, `SELECT count(*) FROM events`) != events {
				t.Fatal("failed publication did not roll back")
			}
			d := s.due[lifecycleKey{attemptProject, 2}]
			if d.lastErrorCode != "stored_"+mode+"_invalid" || !d.successAt.IsZero() {
				t.Fatal("missing hold or fabricated success", d)
			}
			if len(s.lanes[lifecycleMetadata].projects) != 0 || len(s.lanes[lifecycleMetadata].accounts) != 0 {
				t.Fatal("hold leaked lane reservation")
			}
		})
	}
}

func TestLifecycleDatabaseAndSharedAuthorityFailuresRemainFatal(t *testing.T) {
	for _, mode := range []string{"configuration table", "metadata table", "shared workspace authority"} {
		t.Run(mode, func(t *testing.T) {
			f, s, _ := lifecycleFixture(t)
			r := &refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
				p := lifecyclePull(n)
				p.Title = "must not publish"
				return p, nil
			}}
			readers := map[string]TargetReader{"test-capability": r}
			target, err := f.e.claimLifecycle(t.Context(), readers, s)
			if err != nil || target == nil {
				t.Fatal(target, err)
			}
			switch mode {
			case "configuration table":
				admissionSQL(t, f, `ALTER TABLE review_execution_config RENAME TO unavailable_configuration`)
			case "metadata table":
				admissionSQL(t, f, `ALTER TABLE review_pull_versions RENAME TO unavailable_pull_versions`)
			case "shared workspace authority":
				admissionSQL(t, f, `CREATE TRIGGER stale_workspace_authority AFTER INSERT ON review_snapshots BEGIN UPDATE review_workspace SET revision=NEW.revision WHERE project_id=NEW.project_id; END`)
			}
			before := attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`)
			err = f.e.observeLifecycle(t.Context(), *target, s)
			if err == nil || lifecycleStoredStateCode(err, attemptProject, 2) != "" {
				t.Fatal("genuine persistence failure isolated as display corruption", err)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`) != before {
				t.Fatal("failed persistence left partial snapshot")
			}
			if d := s.due[lifecycleKey{attemptProject, 2}]; !d.successAt.IsZero() {
				t.Fatal("rolled-back publication marked successful", d)
			}
		})
	}
}

func TestLifecycleStoredStateClassificationDoesNotMaskCancellation(t *testing.T) {
	f, s, _ := lifecycleFixture(t)
	r := &refreshTestReader{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if target, err := f.e.claimLifecycle(ctx, map[string]TargetReader{"test-capability": r}, s); err == nil || target != nil {
		t.Fatal("parent cancellation held as data", target, err)
	}
	// Only errors constructed at a retained display/config decoding boundary
	// qualify. An identical public storage code is insufficient evidence.
	if lifecycleStoredStateCode(core.Fail("storage_unavailable", "Retained review history is inconsistent.", 500), attemptProject, 2) != "" {
		t.Fatal("shared authority error swallowed")
	}
	tx, err := f.store.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = readLifecycleConfiguration(ctx, tx, attemptProject)
	if !errors.Is(err, context.Canceled) || lifecycleStoredStateCode(err, attemptProject, 2) != "" {
		t.Fatal("configuration cancellation swallowed", err)
	}
}

func TestLifecycleStoredStateHoldPreservesNewerComparison(t *testing.T) {
	c, err := lifecycleOptions(nil)
	if err != nil {
		t.Fatal(err)
	}
	s := newLifecycleSchedule(c)
	key := lifecycleKey{attemptProject, 2}
	s.due[key] = lifecycleDue{head: "newer-head", baseSHA: "newer-base", baseRef: "main"}
	_, failure := decodeLifecycleConfiguration(attemptProject, "{}")
	s.holdStoredState(attemptProject, 2, lifecycleEnrichment, lifecycleComparison{head: "claimed-head", baseSHA: "claimed-base", baseRef: "main"}, failure)
	d := s.due[key]
	if d.head != "newer-head" || d.enrichmentHead != "claimed-head" || d.enrichmentBaseSHA != "claimed-base" {
		t.Fatal("scoped hold swallowed concurrent metadata movement", d)
	}
}
