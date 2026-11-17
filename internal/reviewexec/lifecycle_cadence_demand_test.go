package reviewexec

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func cadenceDemandDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{`CREATE TABLE review_operations(project_id TEXT,pull_number INTEGER,state TEXT)`, `CREATE INDEX op_pending ON review_operations(state)`, `CREATE TABLE review_selections(project_id TEXT,expected_pull_number INTEGER,state TEXT)`, `CREATE INDEX sel_pending ON review_selections(state)`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}
func readDemandTest(t *testing.T, db *sql.DB) cadenceDemand {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	return readCadenceDemand(t.Context(), tx)
}
func TestCadenceDemandPendingAndLateBound(t *testing.T) {
	db := cadenceDemandDB(t)
	for _, q := range []string{`INSERT INTO review_operations VALUES('p',1,'active'),('p',2,'completed'),('p',3,'uncertain'),('other',1,'queued')`, `INSERT INTO review_selections VALUES('p',0,'selecting'),('other',2,'queued'),('ignored',0,'held')`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	d := readDemandTest(t, db)
	if !d.complete || !d.projects["p"] || d.projects["ignored"] || !d.targets[lifecycleKey{"p", 1}] || d.targets[lifecycleKey{"p", 2}] || !d.targets[lifecycleKey{"other", 2}] {
		t.Fatalf("%+v", d)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM review_operations`).Scan(&n); err != nil || n != 4 {
		t.Fatal(n, err)
	}
}
func TestCadenceDemandErrorsAndOverflowUnknown(t *testing.T) {
	for _, table := range []string{"review_operations", "review_selections"} {
		t.Run(table, func(t *testing.T) {
			db := cadenceDemandDB(t)
			for i := 0; i < 257; i++ {
				if _, err := db.Exec(`INSERT INTO ` + table + ` VALUES('p',1,'queued')`); err != nil {
					t.Fatal(err)
				}
			}
			if d := readDemandTest(t, db); d.complete {
				t.Fatal("overflow admitted absence")
			}
		})
	}
	db := cadenceDemandDB(t)
	db.Exec(`DROP TABLE review_selections`)
	if readDemandTest(t, db).complete {
		t.Fatal("missing domain known")
	}
	tx, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if readCadenceDemand(ctx, tx).complete {
		t.Fatal("cancel known")
	}
}
func TestCadenceDemandJoinConservative(t *testing.T) {
	stable := lifecycleDue{head: "h", ciHead: "h", baseSHA: "b", ciBaseSHA: "b", baseRef: "main", ciBaseRef: "main", ciGateState: "success"}
	s := &lifecycleSchedule{due: map[lifecycleKey]lifecycleDue{{"p", 1}: stable, {"p", 2}: stable, {"other", 1}: stable}}
	x := s.joinCadenceDemandLocked(cadenceDemand{})
	if x.Unknown != 3 || x.LocalQuietNoDurableDemand != 0 {
		t.Fatal(x)
	}
	x = s.joinCadenceDemandLocked(cadenceDemand{complete: true, projects: map[string]bool{"p": true}})
	if x.Demanded != 2 || x.UnboundProject != 2 || x.LocalQuietNoDurableDemand != 1 {
		t.Fatal(x)
	}
}
func TestCadenceDemandHookCannotChangeObservationResult(t *testing.T) {
	f, s, _ := lifecycleFixture(t)
	// Missing optional selection domain makes diagnostics unknown, not an error
	// returned to the caller or an authority to dispatch/provider-read.
	s.options.CadenceShadow = true
	f.e.observeCadenceDemand(t.Context(), s)
	next := s.demandShadowNext
	if next.IsZero() {
		t.Fatal("no diagnostic throttle")
	}
	f.e.observeCadenceDemand(t.Context(), s)
	if s.demandShadowNext != next {
		t.Fatal("throttle changed")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f.e.observeCadenceDemand(ctx, s)
	if s.demandShadowNext != next {
		t.Fatal("canceled diagnostic changed state")
	}
}

func TestCadenceDemandCannotSkipStoredStateHold(t *testing.T) {
	f, s, clock := lifecycleFixture(t)
	s.options.CadenceShadow = true
	reader := externalReader(externalSnapshot(0, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	target, err := f.e.claimLifecycle(t.Context(), map[string]TargetReader{"test-capability": reader}, s)
	if err != nil || target == nil {
		t.Fatal(target, err)
	}
	admissionSQL(t, f, `UPDATE review_execution_config SET body_json='{"queue":"bad"}' WHERE project_id=?`, attemptProject)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	// Inject expiry at the first time read after finish. With the old ordering
	// this was the diagnostic and the subsequent ctx gate skipped the hold.
	s.options.Now = func() time.Time {
		calls++
		if calls == 2 {
			cancel()
		}
		return clock.Now()
	}
	err = f.e.observeLifecycle(ctx, *target, s)
	if err != nil {
		t.Fatal("diagnostic boundary changed isolated result", err)
	}
	due := s.due[lifecycleKey{target.project, target.number}]
	if due.lastErrorCode != "stored_configuration_invalid" && due.enrichmentError != "stored_configuration_invalid" {
		t.Fatalf("protective hold lost: %+v", due)
	}
	if reader.calls.Load() != 0 {
		t.Fatal("corrupt configuration reached provider")
	}
}
