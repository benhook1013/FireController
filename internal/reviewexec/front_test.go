package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"github.com/benhook1013/FireController/internal/core"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func frontFixture(t *testing.T) *attemptFixture {
	t.Helper()
	f := attemptProjectionFixture(t, true)
	admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'selection-worker','Selector')`, attemptProject)
	return f
}
func frontRead(t *testing.T, f *attemptFixture) FrontSnapshot {
	t.Helper()
	value, err := f.e.ReadFronts(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"))
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func frontChannel(t *testing.T, value FrontSnapshot, channel string) FrontChannel {
	t.Helper()
	for _, c := range value.Channels {
		if c.Channel == channel {
			return c
		}
	}
	t.Fatal("missing channel", channel)
	return FrontChannel{}
}
func frontSelect(t *testing.T, f *attemptFixture, key, channel string, reader TargetReader) {
	t.Helper()
	expected := int64(99)
	r, _ := selectionEnqueue(t, f, key, channel, &expected)
	selectionNext(t, f, reader)
	if got := selectionRead(t, f, r.ID); got.State != "held" || got.Reason != "expected_pr_mismatch" || got.OperationID != "" {
		t.Fatal("assertion should not dispatch", got)
	}
}
func frontClose(t *testing.T, f *attemptFixture, n int64, channel, key string) {
	t.Helper()
	zero := int64(0)
	if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), key, n, channel, RoundControlRequest{ExpectedRevision: &zero, Action: "close"}); err != nil {
		t.Fatal(err)
	}
}
func TestFrontsSelectionPrefixCurrentPolicyAndIndependentChannels(t *testing.T) {
	f := frontFixture(t)
	reader := &refreshTestReader{}
	frontClose(t, f, 2, "cli", "front-close-cli-two")
	frontSelect(t, f, "front-cli", "cli", reader)
	frontSelect(t, f, "front-hosted", "hosted", reader)
	if reader.calls.Load() != 5 {
		t.Fatal("extra provider reads", reader.calls.Load())
	}
	before := selectionCounts(t, f)
	value := frontRead(t, f)
	cli, hosted := frontChannel(t, value, "cli"), frontChannel(t, value, "hosted")
	if !value.Configured || value.SnapshotID == "" || value.RepositoryID != 801 || value.ConfigurationRevision != 1 || value.PolicyRevision != 1 || len(value.Queue) != 2 || cli.State != "ready" || cli.PullNumber != 4 || hosted.State != "ready" || hosted.PullNumber != 2 || cli.CheckedAt == "" || cli.SourceObservedAt == "" || cli.HeadSHA != strings.Repeat("a", 40) || cli.BaseSHA != strings.Repeat("b", 40) || cli.BaseRef != "main" {
		t.Fatal(value)
	}
	if selectionCounts(t, f) != before || reader.calls.Load() != 5 {
		t.Fatal("read changed authority/provider counts")
	}
	// Local closure immediately changes the cached assessment, not its source frame.
	frontClose(t, f, 4, "cli", "front-close-cli-four")
	if got := frontChannel(t, frontRead(t, f), "cli"); got.State != "complete" {
		t.Fatal(got)
	}
	frontClose(t, f, 2, "hosted", "front-close-hosted-two")
	if got := frontChannel(t, frontRead(t, f), "hosted"); got.State != "unknown" || got.Reason != "source_prefix_incomplete" {
		t.Fatal("incomplete suffix invented completion", got)
	}
	if err := ValidateFrontSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal("old source frame invalidated by current controls", err)
	}
	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_front_frames WHERE project_id=?`, attemptProject).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
}
func TestFrontsSourceFailureReplacesSuccessAndRetainsNoRichProviderData(t *testing.T) {
	f := frontFixture(t)
	reader := &refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		return reviewprovider.Pull{Number: n, RepositoryID: 801, State: "open", Title: "PRIVATE_TITLE_SENTINEL", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}
	frontSelect(t, f, "front-good", "cli", reader)
	var old string
	if err := f.store.DB().QueryRow(`SELECT prefix_json FROM review_front_frames WHERE project_id=? AND channel='cli'`, attemptProject).Scan(&old); err != nil || strings.Contains(old, "PRIVATE_TITLE_SENTINEL") || strings.Contains(old, "ChangedFiles") {
		t.Fatal("rich source leaked", err)
	}
	bad := &refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) {
		return reviewprovider.Pull{}, errors.New("PRIVATE_PROVIDER_FAILURE")
	}}
	r, _ := selectionEnqueue(t, f, "front-source-failed", "cli", nil)
	selectionNext(t, f, bad)
	if selectionRead(t, f, r.ID).Reason != "source_unavailable" {
		t.Fatal("unexpected failure")
	}
	got := frontChannel(t, frontRead(t, f), "cli")
	if got.State != "unknown" || got.Reason != "source_unavailable" || got.PullNumber != 0 || got.CheckedAt == "" || got.SourceObservedAt != "" {
		t.Fatal("stale successful front survived", got)
	}
	var body string
	var count int
	if err := f.store.DB().QueryRow(`SELECT prefix_json FROM review_front_frames WHERE project_id=? AND channel='cli'`, attemptProject).Scan(&body); err != nil || body != "[]" {
		t.Fatal(body, err)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_front_frames WHERE project_id=? AND channel='cli'`, attemptProject).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}
func TestFrontsBindingPauseRecoveryAndMetadataFences(t *testing.T) {
	for _, which := range []string{"head", "base", "lifecycle", "retired", "configuration", "repository", "pause", "recovery"} {
		t.Run(which, func(t *testing.T) {
			f := frontFixture(t)
			frontSelect(t, f, "front-before-change", "cli", &refreshTestReader{})
			switch which {
			case "head", "base", "lifecycle":
				p := projectionPull(2)
				if which == "head" {
					p.HeadSHA = strings.Repeat("c", 40)
				}
				if which == "base" {
					p.BaseSHA = strings.Repeat("c", 40)
				}
				if which == "lifecycle" {
					p.State = "closed"
				}
				var revision int64
				if err := f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, attemptProject).Scan(&revision); err != nil {
					t.Fatal(err)
				}
				request := reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 801, Source: "changed-display", ObservedAt: stamp(), Queue: []reviews.PullInput{frontMergedPull(), p, projectionPull(4)}}
				w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "front-visible-change", encoded(request))
				if w.Code != 201 {
					t.Fatal(w.Code, w.Body.String())
				}
			case "retired":
				var revision int64
				if err := f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, attemptProject).Scan(&revision); err != nil {
					t.Fatal(err)
				}
				request := reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 801, Source: "retired-display", ObservedAt: stamp(), Queue: []reviews.PullInput{frontMergedPull(), projectionPull(4)}, Retired: []reviews.RetiredPull{{Number: 2, Reason: "superseded"}}}
				w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "front-retirement", encoded(request))
				if w.Code != 201 {
					t.Fatal(w.Code, w.Body.String())
				}
			case "configuration", "pause":
				f.configure(t, attemptProject, "test-capability", 1, which == "pause")
			case "repository":
				admissionSQL(t, f, `UPDATE projects SET github_repository_id=902 WHERE id=?`, attemptProject)
			case "recovery":
				admissionSQL(t, f, `INSERT INTO metadata(key,value) VALUES('restore:provider_state','held')`)
			}
			got := frontChannel(t, frontRead(t, f), "cli")
			if got.State == "ready" || got.PullNumber != 0 {
				t.Fatal("changed binding reported ready", which, got)
			}
			if which == "pause" || which == "recovery" {
				if got.State != "held" {
					t.Fatal(got)
				}
			}
			if err := ValidateFrontSnapshot(t.Context(), f.store.DB()); err != nil {
				t.Fatal("historical frame rejected after current change", err)
			}
		})
	}
}
func TestFrontsReadIsolationAndMissingSource(t *testing.T) {
	f := frontFixture(t)
	if got := frontChannel(t, frontRead(t, f), "cli"); got.State != "unknown" || got.Reason != "source_frame_unavailable" {
		t.Fatal(got)
	}
	frontSelect(t, f, "front-own", "cli", &refreshTestReader{})
	scope := f.scope(t, otherAttemptProject, f.other, false)
	got, err := f.e.ReadFronts(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if got.Configured || len(got.Queue) != 0 || len(got.Channels) != 0 || got.ProjectID != otherAttemptProject {
		t.Fatal("cross-project source", got)
	}
	scope.ProjectID = attemptProject
	if _, err = f.e.ReadFronts(t.Context(), scope); err == nil {
		t.Fatal("foreign credential accepted")
	}
}

func TestFrontsMalformedAndHistoricalPrefixValidation(t *testing.T) {
	for _, query := range []string{
		`UPDATE review_front_frames SET prefix_json='null'`,
		`UPDATE review_front_frames SET prefix_json=replace(replace(prefix_json,',"visible_at_acquisition":true',''),',"visible_at_acquisition":false','')`,
		`UPDATE review_front_frames SET prefix_json=replace(replace(prefix_json,'"visible_at_acquisition":true','"visible_at_acquisition":"yes"'),'"visible_at_acquisition":false','"visible_at_acquisition":"yes"')`,
		`UPDATE review_front_frames SET prefix_json=' []'`,
		`UPDATE review_front_frames SET prefix_json=replace(prefix_json,'"number":2','"number":4')`,
		`UPDATE review_front_frames SET prefix_json=replace(prefix_json,'"number":2','"number":2,"number":2')`,
		`UPDATE review_front_frames SET checked_at=CAST(checked_at AS BLOB)`,
		`UPDATE review_front_frames SET source_observed_at='unknown'`,
		`UPDATE review_front_frames SET prefix_json=prefix_json||replace(hex(zeroblob(131073)),'0',' ')`,
	} {
		t.Run(query[:min(55, len(query))], func(t *testing.T) {
			f := frontFixture(t)
			frontSelect(t, f, "front-valid", "cli", &refreshTestReader{})
			admissionSQL(t, f, query)
			if err := ValidateFrontSnapshot(t.Context(), f.store.DB()); err == nil {
				t.Fatal("corrupt source cache accepted")
			}
			if got := frontChannel(t, frontRead(t, f), "cli"); got.State != "unknown" || got.Reason != "source_frame_invalid" {
				t.Fatal(got)
			}
		})
	}
}
func TestFrontsEvaluatorRetainsBlockingPRAndAdmissionSemantics(t *testing.T) {
	f := selectionFixture(t)
	plan := SourcePlan{Repository: reviewprovider.Repository{ID: 801}, Pull: reviewprovider.Pull{RepositoryID: 801, Number: 2, State: "closed", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main"}, ParentSHA: strings.Repeat("b", 40), ObservedAt: stamp()}
	if err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		r := Run{ProjectID: attemptProject, Channel: "cli"}
		value, err := f.e.evaluateFront(ctx, tx, r, 801, []SourcePlan{plan}, false)
		if err != nil {
			return err
		}
		if value.state != "held" || value.reason != "front_unavailable" || value.plan == nil || value.plan.Pull.Number != 2 {
			t.Fatal(value)
		}
		n, reason, err := f.e.runFront(ctx, tx, r, 801, []SourcePlan{plan})
		if err != nil {
			return err
		}
		if n != 0 || reason != "front_unavailable" {
			t.Fatal(n, reason)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func frontMergedPull() reviews.PullInput { p := projectionPull(1); p.State = "merged"; return p }

func TestFrontsEvaluatorIncomingRouteAndPendingEvidenceNamesBlocker(t *testing.T) {
	f, sourceID := runRouteSetup(t)
	routeCreate(t, f, sourceID, "front-incoming-route", routeRequest(0, 0, 4))
	plans := []SourcePlan{{Repository: reviewprovider.Repository{ID: 801}, Pull: reviewprovider.Pull{RepositoryID: 801, Number: 4, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main"}, ParentSHA: strings.Repeat("b", 40), ObservedAt: stamp()}}
	if err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		value, err := f.e.evaluateFront(ctx, tx, Run{ProjectID: attemptProject, Channel: "cli"}, 801, plans, false)
		if err != nil {
			return err
		}
		if value.state != "held" || value.reason != "routed_findings_pending" || value.plan == nil || value.plan.Pull.Number != 4 {
			t.Fatal("receiving route lost its blocker", value)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Real completed journal evidence awaiting capture/adjudication holds even
	// without browser history; the diagnostic preserves the exact predecessor.
	g := selectionFixture(t)
	a := g.start(t, attemptProject, g.enqueue(t, attemptProject, "front-pending-capture", "cli", 2, 1))
	anchor := attemptAnchor(attemptProject, 2)
	anchor.ParentSHA = anchor.BaseSHA
	var err error
	a, err = g.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = g.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "synthetic_missing_capture", ""); err != nil {
		t.Fatal(err)
	}
	plans[0].Pull.Number = 2
	if err = g.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		value, err := g.e.evaluateFront(ctx, tx, Run{ProjectID: attemptProject, Channel: "cli"}, 801, plans, false)
		if err != nil {
			return err
		}
		if value.state != "held" || value.reason != "review_evidence_pending" || value.plan == nil || value.plan.Pull.Number != 2 {
			t.Fatal("pending evidence lost blocker", value)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestFrontsFrameAndSelectionCommitAtomically(t *testing.T) {
	f := frontFixture(t)
	expected := int64(99)
	r, _ := selectionEnqueue(t, f, "front-atomic-failure", "cli", &expected)
	admissionSQL(t, f, `CREATE TRIGGER reject_front_final BEFORE UPDATE OF state ON review_selections WHEN NEW.state='held' BEGIN SELECT RAISE(ABORT,'synthetic front final failure'); END`)
	worked, err := f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": &refreshTestReader{}})
	if err == nil || !worked {
		t.Fatal("final fault did not fail", worked, err)
	}
	var n int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_front_frames`).Scan(&n); err != nil || n != 0 {
		t.Fatal("cache escaped failed selection", n, err)
	}
	if got := selectionRead(t, f, r.ID); got.State != "selecting" {
		t.Fatal("partial final selection persisted", got)
	}
}
func TestFrontsRestartAndPrimaryKeyRead(t *testing.T) {
	f := frontFixture(t)
	frontSelect(t, f, "front-before-restart", "cli", &refreshTestReader{})
	before := frontRead(t, f)
	rows, err := f.store.DB().Query(`EXPLAIN QUERY PLAN SELECT prefix_json FROM review_front_frames WHERE project_id=? AND channel=?`, attemptProject, "cli")
	if err != nil {
		t.Fatal(err)
	}
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			break
		}
		details = append(details, detail)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(details, " ")
	if !strings.Contains(plan, "USING INDEX") || strings.Contains(plan, "SCAN ") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatal("cache read not PK bounded", plan)
	}
	var seq int
	var dbName, database string
	if err = f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &dbName, &database); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := core.Open(t.Context(), filepath.Dir(database))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err = EnsureSchema(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	f.store = reopened
	f.e, err = New(reopened, attemptCapabilities, WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	if after := frontRead(t, f); !reflect.DeepEqual(before, after) {
		t.Fatal("restart changed as-of facts", before, after)
	}
}

func frontSelectiveFixture(t *testing.T) *attemptFixture {
	t.Helper()
	f := attemptProjectionFixture(t, false)
	admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'selection-worker','Selector')`, attemptProject)
	request := reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: 801, Source: "selected-history-only", ObservedAt: stamp(), Queue: []reviews.PullInput{projectionPull(4)}}
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "front-selective-projection", encoded(request))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	return f
}
func frontMergedPredecessorReader() *refreshTestReader {
	return &refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		p := reviewprovider.Pull{Number: n, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}
		if n == 2 {
			p.State = "closed"
			p.Merged = true
		}
		return p, nil
	}}
}
func TestFrontsOmittedMergedHistoryDoesNotRequireImport(t *testing.T) {
	f := frontSelectiveFixture(t)
	reader := frontMergedPredecessorReader()
	frontSelect(t, f, "front-omitted-predecessor", "cli", reader)
	if got := frontChannel(t, frontRead(t, f), "cli"); got.State != "ready" || got.PullNumber != 4 {
		t.Fatal("omitted old merged PR blocked selected visible front", got)
	}
	frame, found, err := readFrontFrame(t.Context(), f.store.DB(), attemptProject, "cli")
	if err != nil || !found || len(frame.prefix) != 2 || frame.prefix[0].VisibleAtAcquisition || !frame.prefix[1].VisibleAtAcquisition || reader.calls.Load() != 3 {
		t.Fatal("acquisition visibility not local/exact", frame, err, reader.calls.Load())
	}
	var n int
	if err = f.store.DB().QueryRow(`SELECT count(*) FROM review_current_pulls WHERE project_id=? AND number=2`, attemptProject).Scan(&n); err != nil || n != 0 {
		t.Fatal("read imported unwanted history", n, err)
	}
	var revision int64
	if err = f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, attemptProject).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	request := reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 801, Source: "retire-visible-target", ObservedAt: stamp(), Queue: []reviews.PullInput{}, Retired: []reviews.RetiredPull{{Number: 4, Reason: "superseded"}}}
	w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "front-visible-target-retired", encoded(request))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if got := frontChannel(t, frontRead(t, f), "cli"); got.State != "unknown" || got.Reason != "visible_source_changed" {
		t.Fatal("visible target retirement overridden", got)
	}
	if err = ValidateFrontSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal("source fact invalidated by later retirement", err)
	}
}
func TestFrontsNewlyVisiblePredecessorMustMatchCachedSource(t *testing.T) {
	for _, contradiction := range []bool{false, true} {
		t.Run(map[bool]string{false: "matching", true: "contradiction"}[contradiction], func(t *testing.T) {
			f := frontSelectiveFixture(t)
			frontSelect(t, f, "front-before-new-visible", "cli", frontMergedPredecessorReader())
			p := projectionPull(2)
			if !contradiction {
				p.State = "merged"
			}
			var revision int64
			if err := f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, attemptProject).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			request := reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 801, Source: "new-visible-predecessor", ObservedAt: stamp(), Queue: []reviews.PullInput{p, projectionPull(4)}}
			w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "front-new-visible-predecessor", encoded(request))
			if w.Code != 201 {
				t.Fatal(w.Code, w.Body.String())
			}
			got := frontChannel(t, frontRead(t, f), "cli")
			if contradiction {
				if got.State != "unknown" || got.Reason != "visible_source_changed" {
					t.Fatal(got)
				}
			} else if got.State != "ready" || got.PullNumber != 4 {
				t.Fatal(got)
			}
		})
	}
}
func TestFrontsHiddenBlockerAndUnprojectedTargetAreDistinct(t *testing.T) {
	f := frontSelectiveFixture(t)
	reader := &refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
		return reviewprovider.Pull{Number: n, RepositoryID: 801, State: "closed", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}
	r, _ := selectionEnqueue(t, f, "hidden-blocking-predecessor", "cli", nil)
	selectionNext(t, f, reader)
	if selectionRead(t, f, r.ID).Reason != "front_unavailable" {
		t.Fatal("expected closed-front hold")
	}
	if got := frontChannel(t, frontRead(t, f), "cli"); got.State != "held" || got.PullNumber != 2 || got.Reason != "front_unavailable" {
		t.Fatal("hidden source blocker disappeared", got)
	}
	g := selectionFixture(t)
	frontSelect(t, g, "no-projection-target", "cli", &refreshTestReader{})
	if got := frontChannel(t, frontRead(t, g), "cli"); got.State != "unknown" || got.PullNumber != 0 || got.Reason != "workspace_unavailable" {
		t.Fatal("unprojected target claimed ready", got)
	}
}

func TestFrontsCorruptSourceObligationIsHeldAndWorkerContinues(t *testing.T) {
	f := frontFixture(t)
	retainObligationSource(t, f, attemptProject, "front-source-importer", "front-source", strings.Repeat("c", 64), 0)
	// Keep the genuine immutable source and original bytes. This schema-valid
	// revision cannot describe an open row without a resolution; reads must refuse it.
	admissionSQL(t, f, `UPDATE review_adoption_obligations SET revision=2 WHERE project_id=?`, attemptProject)
	var original string
	if err := f.store.DB().QueryRow(`SELECT original_json FROM review_adoption_obligations WHERE project_id=?`, attemptProject).Scan(&original); err != nil {
		t.Fatal(err)
	}
	first, _ := selectionEnqueue(t, f, "corrupt-source-front", "cli", nil)
	reader := &refreshTestReader{}
	ctx, cancel := context.WithCancel(t.Context())
	done, err := f.e.StartReviewRuns(ctx, map[string]TargetReader{"test-capability": reader})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("selection worker shutdown", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("selection worker did not stop")
		}
	})
	wait := func(id string) Run {
		t.Helper()
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			got := selectionRead(t, f, id)
			if !runPending(got.State) {
				return got
			}
			select {
			case err := <-done:
				t.Fatalf("selection worker stopped on diagnostic hold: %v", err)
			case <-deadline.C:
				t.Fatal("selection did not settle")
			case <-tick.C:
			}
		}
	}
	got := wait(first.ID)
	if got.State != "held" || got.Reason != "source_obligations_unavailable" || got.OperationID != "" {
		t.Fatal("corrupt obligation was not safely held", got)
	}
	if front := frontChannel(t, frontRead(t, f), "cli"); front.State != "held" || front.Reason != "source_obligations_unavailable" || front.PullNumber != 2 {
		t.Fatal("front lost the retained blocker", front)
	}
	// A different selected PR can progress through the same running workers;
	// the damaged predecessor is retained untouched, not repaired or bypassed.
	one := int64(1)
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "next-front-config", Configuration{ExpectedRevision: &one, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{4}, Channels: []string{"cli", "hosted"}}); err != nil {
		t.Fatal(err)
	}
	next, _ := selectionEnqueue(t, f, "eligible-after-corrupt-source", "cli", nil)
	got = wait(next.ID)
	if got.State != "selected" || got.OperationID == "" {
		t.Fatal("next eligible selection did not progress", got)
	}
	op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), got.OperationID)
	if err != nil || op.PullNumber != 4 || op.State != "queued" {
		t.Fatal("unexpected child operation", op, err)
	}
	var revision int64
	var after string
	if err := f.store.DB().QueryRow(`SELECT revision,original_json FROM review_adoption_obligations WHERE project_id=?`, attemptProject).Scan(&revision, &after); err != nil || revision != 2 || after != original {
		t.Fatal("selection rewrote corrupt retained obligation", revision, err)
	}
	if reader.calls.Load() != 4 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
		t.Fatal("selection performed review work or repeated source reads", reader.calls.Load())
	}
}
