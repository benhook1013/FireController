package reviewexec

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/adoptionholds"
	"github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/roundadoptionmodel"
)

func adoptionPrepared() roundadoptionmodel.Prepared {
	zero := int64(0)
	return roundadoptionmodel.Prepared{Version: 1, RepositoryID: 801, ExpectedRevision: &zero, PullNumber: 2, Channel: "cli", Source: "sealed-source", SourceKey: "pr2-cli", SourceSHA256: strings.Repeat("a", 64), MaximumRemaining: 4, Taper: 3, InitialDry: 2}
}
func adoptRound(t *testing.T, f *attemptFixture, p roundadoptionmodel.Prepared, key string) []byte {
	t.Helper()
	scope := adoptionScope(t, f, f.admin, true)
	before := roundTables(t, f)
	preview, err := f.e.PreviewRoundAdoption(t.Context(), scope, p)
	if err != nil {
		t.Fatal(err)
	}
	if roundTables(t, f) != before {
		t.Fatal("preview mutated round/audit state")
	}
	result, err := f.e.ApplyRoundAdoption(t.Context(), scope, key, roundadoptionmodel.ApplyRequest{PreviewSHA256: preview.PreviewSHA256, Control: p})
	if err != nil {
		t.Fatal(err)
	}
	var receipt RoundControlReceipt
	if json.Unmarshal(result, &receipt) != nil || receipt.Outcome != "adopted" || receipt.Revision != 1 || receipt.Epoch != 1 {
		t.Fatal("invalid adoption receipt")
	}
	return result
}
func adoptionValid(t *testing.T, f *attemptFixture) {
	t.Helper()
	if err := ValidateRoundSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
}
func TestRoundAdoptionCarriedDryNativeCompletionAndReset(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprint(accepted), func(t *testing.T) {
			f := adoptionFixture(t)
			p := adoptionPrepared()
			adoptRound(t, f, p, "adopt")
			c := roundRead(t, f, 2, "cli")
			if c.Used != 0 || c.Dry != 2 || c.InitialDry != 2 || c.Tapered || c.OriginSHA256 != p.SourceSHA256 {
				t.Fatal("carried baseline incorrect", c)
			}
			for _, table := range []string{"review_execution_attempts", "review_execution_evidence", "review_round_completions", "review_round_operation_bindings"} {
				if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != 0 {
					t.Fatal("adoption manufactured native authority")
				}
			}
			adoptionValid(t, f)
			a, id := roundCapture(t, f, 0, accepted)
			roundFinalize(t, f, a, id, 0, accepted)
			c = roundRead(t, f, 2, "cli")
			if c.Used != 1 || c.InitialDry != 2 || (!accepted && (c.Dry != 3 || !c.Tapered)) || (accepted && (c.Dry != 0 || c.Tapered)) {
				t.Fatal("native completion did not apply seed/reset", c)
			}
			adoptionValid(t, f)
			one := int64(1)
			if _, err := f.e.ConfigureRoundControl(t.Context(), adoptionScope(t, f, f.admin, true), "close", 2, "cli", RoundControlRequest{ExpectedRevision: &one, Action: "close"}); err != nil {
				t.Fatal(err)
			}
			closed := roundRead(t, f, 2, "cli")
			if !closed.Closed || closed.InitialDry != 2 || closed.OriginSource != p.Source {
				t.Fatal("close lost provenance")
			}
			adoptionValid(t, f)
			roundGrant(t, f, 2, "cli", 2, 1, 2, "replacement")
			c = roundRead(t, f, 2, "cli")
			if c.InitialDry != 0 || c.InitiallyTapered || c.OriginSource != "" || c.OriginKey != "" || c.OriginSHA256 != "" || c.Dry != 0 || c.Tapered || c.Taper != 3 {
				t.Fatal("replacement retained old carry", c)
			}
			adoptionValid(t, f)
		})
	}
}
func TestRoundAdoptionLimitsClosedLatchAndExactMinimum(t *testing.T) {
	for _, kind := range []string{"exhausted", "closed", "sticky", "custom", "minimum", "exact"} {
		t.Run(kind, func(t *testing.T) {
			f := adoptionFixture(t)
			p := adoptionPrepared()
			switch kind {
			case "exhausted":
				p.MaximumRemaining = 0
			case "closed":
				p.Closed = true
			case "sticky":
				p.Tapered = true
				p.InitialDry = 0
			case "custom":
				p.Taper = 2
				p.InitialDry = 1
			case "minimum":
				p.MinimumRemaining = 2
			case "exact":
				p.MinimumRemaining = 2
				p.MaximumRemaining = 2
			}
			adoptRound(t, f, p, "selected")
			adoptionValid(t, f)
			if kind == "exhausted" || kind == "closed" || kind == "sticky" {
				_, err := f.e.Enqueue(t.Context(), adoptionScope(t, f, f.writer, false), "must-not-dispatch", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
				code := "review_round_complete"
				if kind == "closed" {
					code = "review_round_closed"
				}
				attemptCode(t, err, code)
				if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
					t.Fatal("finished/closed rule queued work")
				}
				return
			}
			a, id := roundCapture(t, f, 0, false)
			roundFinalize(t, f, a, id, 0, false)
			c := roundRead(t, f, 2, "cli")
			if kind == "custom" {
				if !c.Tapered || c.Dry != 2 || c.Used != 1 {
					t.Fatal("custom taper ignored", c)
				}
			} else {
				if c.Tapered || c.Used != 1 {
					t.Fatal("minimum was bypassed")
				}
				a, id = roundCapture(t, f, 1, false)
				roundFinalize(t, f, a, id, 1, false)
				c = roundRead(t, f, 2, "cli")
				if !c.Tapered || c.Used != 2 {
					t.Fatal("required rounds not honored", c)
				}
			}
			adoptionValid(t, f)
		})
	}
}
func TestRoundAdoptionReplayAdmissionAndAtomicity(t *testing.T) {
	f := adoptionFixture(t)
	p := adoptionPrepared()
	admin := adoptionScope(t, f, f.admin, true)
	preview, err := f.e.PreviewRoundAdoption(t.Context(), admin, p)
	if err != nil {
		t.Fatal(err)
	}
	request := roundadoptionmodel.ApplyRequest{PreviewSHA256: preview.PreviewSHA256, Control: p}
	before := roundTables(t, f)
	if _, err = f.store.DB().Exec(`CREATE TRIGGER adoption_failure BEFORE INSERT ON review_round_adoptions BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.ApplyRoundAdoption(t.Context(), admin, "recover", request); err == nil {
		t.Fatal("injected failure succeeded")
	}
	if before != roundTables(t, f) || attemptCount(t, f.store, `SELECT count(*) FROM review_round_adoptions`) != 0 {
		t.Fatal("failed adoption partially committed")
	}
	if _, err = f.store.DB().Exec(`DROP TRIGGER adoption_failure`); err != nil {
		t.Fatal(err)
	}
	original, err := f.e.ApplyRoundAdoption(t.Context(), admin, "recover", request)
	if err != nil {
		t.Fatal(err)
	}
	roundGrant(t, f, 2, "cli", 1, 0, 5, "later")
	before = roundTables(t, f)
	replay, err := f.e.ApplyRoundAdoption(t.Context(), admin, "recover", request)
	if err != nil || string(replay) != string(original) || before != roundTables(t, f) {
		t.Fatal("original replay altered later settings", err)
	}
	_, err = f.e.ApplyRoundAdoption(t.Context(), admin, "new-key", request)
	attemptCode(t, err, "review_round_revision_conflict")
	changed := request
	changed.Control.InitialDry = 1
	_, err = f.e.ApplyRoundAdoption(t.Context(), admin, "recover", changed)
	attemptCode(t, err, "import_preview_mismatch")
	_, err = f.e.ApplyRoundAdoption(t.Context(), adoptionScope(t, f, f.writer, false), "writer", request)
	attemptCode(t, err, "forbidden")
	// A fresh preview for another PR cannot reuse the original source identity.
	other := p
	other.PullNumber = 4
	_, err = f.e.PreviewRoundAdoption(t.Context(), admin, other)
	attemptCode(t, err, "review_round_adoption_conflict")
	if _, err = f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE principal_id=?`, admin.PrincipalID); err != nil {
		t.Fatal(err)
	}
	_, err = f.e.ApplyRoundAdoption(t.Context(), admin, "recover", request)
	if err == nil {
		t.Fatal("revoked original receipt replay succeeded")
	}
}
func TestRoundAdoptionAdmissionRecheckedAfterPreview(t *testing.T) {
	for _, kind := range []string{"repository", "queue", "pending"} {
		t.Run(kind, func(t *testing.T) {
			f := adoptionFixture(t)
			p := adoptionPrepared()
			scope := adoptionScope(t, f, f.admin, true)
			preview, err := f.e.PreviewRoundAdoption(t.Context(), scope, p)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "repository":
				if _, err = f.store.DB().Exec(`UPDATE projects SET github_repository_id=999 WHERE id=?`, attemptProject); err != nil {
					t.Fatal(err)
				}
			case "queue":
				one := int64(1)
				_, err = f.e.Configure(t.Context(), scope, "new-queue", Configuration{ExpectedRevision: &one, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{4}, Channels: []string{"cli", "hosted"}})
				if err != nil {
					t.Fatal(err)
				}
			case "pending":
				f.enqueue(t, attemptProject, "already-pending", "cli", 2, 1)
			}
			_, err = f.e.ApplyRoundAdoption(t.Context(), scope, "adopt", roundadoptionmodel.ApplyRequest{PreviewSHA256: preview.PreviewSHA256, Control: p})
			if err == nil {
				t.Fatal("stale admission succeeded")
			}
			if roundRead(t, f, 2, "cli").Configured || attemptCount(t, f.store, `SELECT count(*) FROM review_round_adoptions`) != 0 {
				t.Fatal("admission refusal mutated controls")
			}
		})
	}
}
func TestRoundAdoptionHTTPStrictShape(t *testing.T) {
	f := adoptionFixture(t)
	path := "/api/v1/projects/" + attemptProject + "/reviews/rounds/adoption/preview"
	body := encoded(adoptionPrepared())
	result := f.request(http.MethodPost, path, f.admin, "", body)
	if result.Code != 200 {
		t.Fatalf("valid preview: %d %s", result.Code, result.Body.String())
	}
	for _, bad := range []string{strings.Replace(body, `"closed":false`, `"closed":null`, 1), strings.Replace(body, `"closed":false`, `"closed":false,"closed":false`, 1), strings.Replace(body, `,"closed":false`, "", 1), strings.Replace(body, `"version":1`, `"version":1,"other":0`, 1), strings.Replace(body, `"initial_dry":2`, `"initial_dry":1001`, 1)} {
		if got := f.request(http.MethodPost, path, f.admin, "", bad); got.Code != 400 {
			t.Fatalf("invalid shape accepted status %d", got.Code)
		}
	}
}
func TestRoundAdoptionVersionOneUpgradeConservesHistory(t *testing.T) {
	f := adoptionFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 4, "old-native")
	a, id := roundCapture(t, f, 0, false)
	roundFinalize(t, f, a, id, 0, false)
	original := roundRead(t, f, 2, "cli")
	var audit string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_round_changes`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{`DROP TABLE review_round_adoptions`, `DELETE FROM metadata WHERE key='schema:review_round_adoption'`, `ALTER TABLE review_round_controls RENAME TO fixture_controls_v2`, RoundSchemaV1SQL()[0], `INSERT INTO review_round_controls(` + roundColumnsV1 + `) SELECT ` + roundColumnsV1 + ` FROM fixture_controls_v2`, `DROP TABLE fixture_controls_v2`, `ALTER TABLE review_round_completions RENAME TO fixture_completions_v3`, RoundSchemaV1SQL()[4], `INSERT INTO review_round_completions SELECT * FROM fixture_completions_v3`, `DROP TABLE fixture_completions_v3`, `UPDATE metadata SET value='1' WHERE key='schema:review_rounds'`} {
		if _, err = tx.ExecContext(t.Context(), sql); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = ValidateRoundSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal("old snapshot rejected", err)
	}
	oldRows := roundTables(t, f)
	var oldDDL string
	if err = f.store.DB().QueryRow(`SELECT sql FROM sqlite_schema WHERE name='review_round_controls'`).Scan(&oldDDL); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.DB().Exec(`CREATE TRIGGER migration_fixture_failure BEFORE UPDATE OF value ON metadata WHEN NEW.key='schema:review_rounds' AND NEW.value='2' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if err = EnsureRoundSchema(t.Context(), f.store); err == nil {
		t.Fatal("injected migration failure succeeded")
	}
	var version, afterDDL string
	if err = f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_rounds'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err = f.store.DB().QueryRow(`SELECT sql FROM sqlite_schema WHERE name='review_round_controls'`).Scan(&afterDDL); err != nil {
		t.Fatal(err)
	}
	if version != "1" || afterDDL != oldDDL || roundTables(t, f) != oldRows || attemptCount(t, f.store, `SELECT count(*) FROM sqlite_schema WHERE name='legacy_review_round_controls'`) != 0 {
		t.Fatal("failed migration did not roll back schema/data/version")
	}
	if _, err = f.store.DB().Exec(`DROP TRIGGER migration_fixture_failure`); err != nil {
		t.Fatal(err)
	}
	if err = EnsureRoundSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if err = ensureRoundAdoptionSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	after := roundRead(t, f, 2, "cli")
	if !reflect.DeepEqual(original, after) {
		t.Fatal("upgrade changed old native state")
	}
	var preserved string
	if err = f.store.DB().QueryRow(`SELECT body_json FROM review_round_changes`).Scan(&preserved); err != nil || audit != preserved {
		t.Fatal("history rewritten")
	}
	adoptionValid(t, f)
	before := roundTables(t, f)
	if err = EnsureRoundSchema(t.Context(), f.store); err != nil || before != roundTables(t, f) {
		t.Fatal("already-current startup altered state")
	}
}

// Canceled contexts leave configuration, provenance, event and receipt writes
// absent; Store owns the ordinary transactional cancellation boundary.
func TestRoundAdoptionCanceledApplyConservesState(t *testing.T) {
	f := adoptionFixture(t)
	p := adoptionPrepared()
	scope := adoptionScope(t, f, f.admin, true)
	preview, err := f.e.PreviewRoundAdoption(t.Context(), scope, p)
	if err != nil {
		t.Fatal(err)
	}
	before := roundTables(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = f.e.ApplyRoundAdoption(ctx, scope, "cancel", roundadoptionmodel.ApplyRequest{PreviewSHA256: preview.PreviewSHA256, Control: p}); err == nil {
		t.Fatal("canceled apply succeeded")
	}
	if before != roundTables(t, f) {
		t.Fatal("canceled apply changed state")
	}
}

func adoptionFixture(t *testing.T) *attemptFixture {
	t.Helper()
	f := roundFixture(t)
	if _, err := f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,'adoption-worker','Importer')`, attemptProject); err != nil {
		t.Fatal(err)
	}
	previous := f.handler
	f.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-FC-Worker", "adoption-worker")
		previous.ServeHTTP(w, r)
	})
	return f
}
func adoptionScope(t *testing.T, f *attemptFixture, token string, admin bool) core.Scope {
	t.Helper()
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("project_id", attemptProject)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-FC-Worker", "adoption-worker")
	var scope core.Scope
	var err error
	if admin {
		scope, err = f.store.AdminScope(r)
	} else {
		scope, err = f.store.Scope(r, "write")
	}
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func obligationSourceScope(t *testing.T, f *attemptFixture, project, worker string) core.Scope {
	t.Helper()
	if _, err := f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,?,?)`, project, worker, "Obligation importer "+worker); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("project_id", project)
	r.Header.Set("Authorization", "Bearer "+f.admin)
	r.Header.Set("X-FC-Worker", worker)
	scope, err := f.store.AdminScope(r)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func retainObligationSource(t *testing.T, f *attemptFixture, project, worker, namespace, snapshot string, revision int64) adoptionholdsmodel.Source {
	t.Helper()
	repository := int64(801)
	if project == otherAttemptProject {
		repository = 802
	}
	source := adoptionholdsmodel.Source{Namespace: namespace, RepositoryID: repository, SnapshotSHA256: snapshot}
	service := adoptionholds.New(f.store)
	scope := obligationSourceScope(t, f, project, worker)
	obligations := []adoptionholdsmodel.Obligation{{SourceKey: "finding-one", Kind: "finding", AffectedPullNumbers: []int64{2}, Summary: "Retained correction", Evidence: []string{}, OriginalStatus: "open"}}
	if revision == 1 {
		obligations = append(obligations, adoptionholdsmodel.Obligation{SourceKey: "route-two", Kind: "route", AffectedPullNumbers: []int64{2}, Summary: "Retained route", Evidence: []string{"source:route-two"}, OriginalStatus: "pending"})
	}
	expected := revision
	batch := adoptionholdsmodel.Batch{Version: 1, Source: source, ExpectedRevision: &expected, Obligations: obligations}
	preview, err := service.Preview(t.Context(), scope, batch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Apply(t.Context(), scope, fmt.Sprintf("retain-%s-%d", project, revision), adoptionholdsmodel.ApplyRequest{Batch: batch, PreviewSHA256: preview.PreviewSHA256}); err != nil {
		t.Fatal(err)
	}
	return source
}

func v2Adoption(source adoptionholdsmodel.Source, revision int64) roundadoptionmodel.Prepared {
	p := adoptionPrepared()
	p.Version = 2
	p.Obligations = &roundadoptionmodel.ObligationsReference{Namespace: source.Namespace, SnapshotSHA256: source.SnapshotSHA256, Revision: revision}
	return p
}

func TestRoundAdoptionV2RequiresMatchingCurrentObligationSource(t *testing.T) {
	t.Run("matching", func(t *testing.T) {
		f := adoptionFixture(t)
		source := retainObligationSource(t, f, attemptProject, "source-worker", "sealed-source", strings.Repeat("b", 64), 0)
		adoptRound(t, f, v2Adoption(source, 1), "v2-matching")
		adoptionValid(t, f)
	})
	for name, setup := range map[string]func(*testing.T, *attemptFixture) roundadoptionmodel.Prepared{
		"missing namespace": func(t *testing.T, f *attemptFixture) roundadoptionmodel.Prepared {
			return v2Adoption(adoptionholdsmodel.Source{Namespace: "sealed-source", RepositoryID: 801, SnapshotSHA256: strings.Repeat("b", 64)}, 1)
		},
		"foreign namespace": func(t *testing.T, f *attemptFixture) roundadoptionmodel.Prepared {
			source := retainObligationSource(t, f, attemptProject, "source-worker", "sealed-source", strings.Repeat("b", 64), 0)
			p := v2Adoption(source, 1)
			p.Obligations.Namespace = "other-source"
			return p
		},
		"foreign project": func(t *testing.T, f *attemptFixture) roundadoptionmodel.Prepared {
			source := retainObligationSource(t, f, otherAttemptProject, "other-source-worker", "sealed-source", strings.Repeat("b", 64), 0)
			source.RepositoryID = 801
			return v2Adoption(source, 1)
		},
		"repository mismatch": func(t *testing.T, f *attemptFixture) roundadoptionmodel.Prepared {
			source := retainObligationSource(t, f, attemptProject, "source-worker", "sealed-source", strings.Repeat("b", 64), 0)
			p := v2Adoption(source, 1)
			p.RepositoryID = 999
			return p
		},
		"snapshot mismatch": func(t *testing.T, f *attemptFixture) roundadoptionmodel.Prepared {
			source := retainObligationSource(t, f, attemptProject, "source-worker", "sealed-source", strings.Repeat("b", 64), 0)
			p := v2Adoption(source, 1)
			p.Obligations.SnapshotSHA256 = strings.Repeat("c", 64)
			return p
		},
		"stale current revision": func(t *testing.T, f *attemptFixture) roundadoptionmodel.Prepared {
			source := retainObligationSource(t, f, attemptProject, "source-worker", "sealed-source", strings.Repeat("b", 64), 0)
			retainObligationSource(t, f, attemptProject, "source-worker-two", "sealed-source", strings.Repeat("b", 64), 1)
			return v2Adoption(source, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := adoptionFixture(t)
			p := setup(t, f)
			before := roundTables(t, f)
			normalized, body, normalizeErr := normalizeRoundAdoption(p)
			if normalizeErr != nil {
				if name != "foreign namespace" || roundTables(t, f) != before {
					t.Fatal("unexpected normalization refusal or mutation", normalizeErr)
				}
				return
			}
			scope := adoptionScope(t, f, f.admin, true)
			preview := f.e.roundAdoptionPreview(scope, normalized, body)
			_, err := f.e.ApplyRoundAdoption(t.Context(), scope, "refuse-"+name, roundadoptionmodel.ApplyRequest{PreviewSHA256: preview.PreviewSHA256, Control: p})
			if err == nil || roundTables(t, f) != before {
				t.Fatal("mismatched source admitted or partially committed", err)
			}
		})
	}
}

func TestRoundAdoptionV2SourceRevisionRaceAndOriginalReplay(t *testing.T) {
	t.Run("advance between preview and apply", func(t *testing.T) {
		f := adoptionFixture(t)
		source := retainObligationSource(t, f, attemptProject, "source-worker", "sealed-source", strings.Repeat("b", 64), 0)
		p := v2Adoption(source, 1)
		scope := adoptionScope(t, f, f.admin, true)
		preview, err := f.e.PreviewRoundAdoption(t.Context(), scope, p)
		if err != nil {
			t.Fatal(err)
		}
		retainObligationSource(t, f, attemptProject, "source-worker-two", "sealed-source", strings.Repeat("b", 64), 1)
		before := roundTables(t, f)
		if _, err = f.e.ApplyRoundAdoption(t.Context(), scope, "stale-preview", roundadoptionmodel.ApplyRequest{PreviewSHA256: preview.PreviewSHA256, Control: p}); err == nil || roundTables(t, f) != before {
			t.Fatal("source revision race admitted or partially committed", err)
		}
	})
	t.Run("original key replay after advance", func(t *testing.T) {
		f := adoptionFixture(t)
		source := retainObligationSource(t, f, attemptProject, "source-worker", "sealed-source", strings.Repeat("b", 64), 0)
		p := v2Adoption(source, 1)
		original := adoptRound(t, f, p, "linked-adoption")
		retainObligationSource(t, f, attemptProject, "source-worker-two", "sealed-source", strings.Repeat("b", 64), 1)
		before := roundTables(t, f)
		scope := adoptionScope(t, f, f.admin, true)
		normalized, body, err := normalizeRoundAdoption(p)
		if err != nil {
			t.Fatal(err)
		}
		preview := f.e.roundAdoptionPreview(scope, normalized, body)
		replayed, err := f.e.ApplyRoundAdoption(t.Context(), scope, "linked-adoption", roundadoptionmodel.ApplyRequest{PreviewSHA256: preview.PreviewSHA256, Control: p})
		if err != nil || string(replayed) != string(original) || roundTables(t, f) != before {
			t.Fatal("original replay re-admitted against advanced source", err)
		}
	})
}

func TestRoundAdoptionV1RemainsIndependentOfObligationSources(t *testing.T) {
	f := adoptionFixture(t)
	p := adoptionPrepared()
	if p.Version != 1 || p.Obligations != nil {
		t.Fatal("fixture no longer exercises v1")
	}
	adoptRound(t, f, p, "v1-unchanged")
	adoptionValid(t, f)
}

func TestRoundAdoptionStickyLatchWaitsForMinimumAfterAcceptedResets(t *testing.T) {
	f := adoptionFixture(t)
	p := adoptionPrepared()
	p.MinimumRemaining = 2
	p.InitialDry = 0
	p.Tapered = true
	adoptRound(t, f, p, "sticky-minimum")
	if c := roundRead(t, f, 2, "cli"); !c.InitiallyTapered || c.Tapered || c.Used != 0 {
		t.Fatal("initial latch bypassed minimum", c)
	}
	adoptionValid(t, f)
	for i := 0; i < 2; i++ {
		a, id := roundCapture(t, f, i, true)
		roundFinalize(t, f, a, id, i, true)
		c := roundRead(t, f, 2, "cli")
		if c.Used != int64(i+1) || c.Dry != 0 || !c.InitiallyTapered || c.Tapered != (i == 1) {
			t.Fatal("accepted reset erased latch or minimum", c)
		}
		adoptionValid(t, f)
	}
}
func TestRoundAdoptionHTTPApplyBindingAndAdmissionBounds(t *testing.T) {
	f := adoptionFixture(t)
	p := adoptionPrepared()
	scope := adoptionScope(t, f, f.admin, true)
	preview, err := f.e.PreviewRoundAdoption(t.Context(), scope, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,'another-worker','Other')`, attemptProject); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("project_id", attemptProject)
	r.Header.Set("Authorization", "Bearer "+f.admin)
	r.Header.Set("X-FC-Worker", "another-worker")
	other, err := f.store.AdminScope(r)
	if err != nil {
		t.Fatal(err)
	}
	input := roundadoptionmodel.ApplyRequest{PreviewSHA256: preview.PreviewSHA256, Control: p}
	_, err = f.e.ApplyRoundAdoption(t.Context(), other, "wrong-binding", input)
	attemptCode(t, err, "import_preview_mismatch")
	path := "/api/v1/projects/" + attemptProject + "/reviews/rounds/adoption/apply"
	if got := f.request(http.MethodPost, path, f.admin, "", encoded(input)); got.Code != 400 {
		t.Fatal("missing apply key accepted", got.Code)
	}
	result := f.request(http.MethodPost, path, f.admin, "http-apply", encoded(input))
	if result.Code != 200 {
		t.Fatal("HTTP apply refused", result.Code)
	}
	again := f.request(http.MethodPost, path, f.admin, "http-apply", encoded(input))
	if again.Code != 200 || again.Body.String() != result.Body.String() {
		t.Fatal("HTTP exact replay changed receipt")
	}
	adoptionValid(t, f)
	for _, change := range []func(*roundadoptionmodel.Prepared){
		func(p *roundadoptionmodel.Prepared) { p.Source = " padded" },
		func(p *roundadoptionmodel.Prepared) { p.SourceKey = "bad\nkey" },
		func(p *roundadoptionmodel.Prepared) { p.Source = string([]byte{0xff}) },
		func(p *roundadoptionmodel.Prepared) { p.SourceSHA256 = strings.Repeat("A", 64) },
		func(p *roundadoptionmodel.Prepared) { v := int64(1); p.ExpectedRevision = &v },
		func(p *roundadoptionmodel.Prepared) { p.MinimumRemaining = 5 },
		func(p *roundadoptionmodel.Prepared) { p.MaximumRemaining = 1001 },
		func(p *roundadoptionmodel.Prepared) { p.Taper = 0 },
	} {
		invalid := adoptionPrepared()
		change(&invalid)
		if _, _, err := normalizeRoundAdoption(invalid); err == nil {
			t.Fatal("unsafe adoption input normalized")
		}
	}
}

func TestRoundAdoptionExistingNativeCaptureNeverBackfillsCredit(t *testing.T) {
	f := adoptionFixture(t)
	a, id := roundCapture(t, f, 0, false)
	roundFinalize(t, f, a, id, 0, false)
	if roundRead(t, f, 2, "cli").Configured {
		t.Fatal("capture configured rounds")
	}
	nativeBefore := []int{attemptCount(t, f.store, `SELECT count(*) FROM review_operations`), attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`), attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`)}
	adoptRound(t, f, adoptionPrepared(), "after-known-capture")
	c := roundRead(t, f, 2, "cli")
	if c.Used != 0 || c.Dry != 2 {
		t.Fatal("historical capture counted as native grant credit", c)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_round_operation_bindings`) != 0 {
		t.Fatal("adoption backfilled old authority")
	}
	nativeAfter := []int{attemptCount(t, f.store, `SELECT count(*) FROM review_operations`), attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`), attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`)}
	if !reflect.DeepEqual(nativeBefore, nativeAfter) {
		t.Fatal("adoption created native records")
	}
	adoptionValid(t, f)
}

func TestRoundAdoptionRejectsUnlatchedCarriedThreshold(t *testing.T) {
	for _, minimum := range []int64{0, 2} {
		t.Run(fmt.Sprint(minimum), func(t *testing.T) {
			f := adoptionFixture(t)
			p := adoptionPrepared()
			p.InitialDry = p.Taper
			p.MinimumRemaining = minimum
			before := roundTables(t, f)
			if _, err := f.e.PreviewRoundAdoption(t.Context(), adoptionScope(t, f, f.admin, true), p); err == nil {
				t.Fatal("reached initial threshold lost its latch")
			}
			if roundTables(t, f) != before {
				t.Fatal("invalid preview mutated retained state")
			}
		})
	}
}
