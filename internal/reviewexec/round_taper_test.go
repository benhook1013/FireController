package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

func historicalRoundTaper(channel string) int64 {
	if channel == "hosted" {
		return 1
	}
	return 3
}

// Retain a native historical rule, not a production threshold mutation. This
// keeps predecessor tests independent of today's enrollment defaults.
func retainHistoricalRoundTaper(t *testing.T, f *attemptFixture, pull int64, channel string) {
	t.Helper()
	roundHook(t, f, func(ctx context.Context, tx *sql.Tx) error {
		c, err := readRoundRecord(ctx, tx, attemptProject, pull, channel)
		if err != nil {
			return err
		}
		if c.Revision != 1 || c.Used != 0 {
			t.Fatal("historical fixture must precede activity")
		}
		c.Taper = historicalRoundTaper(channel)
		if _, err = tx.ExecContext(ctx, `UPDATE review_round_changes SET body_json=? WHERE project_id=? AND pull_number=? AND channel=? AND revision=1`, encodeRoundHistory(c.RoundControl, c.cumulative), attemptProject, pull, channel); err != nil {
			return err
		}
		return storeRoundRecord(ctx, tx, c)
	})
}

func retainRoundV4Changes(t *testing.T, f *attemptFixture) {
	t.Helper()
	roundHook(t, f, func(ctx context.Context, tx *sql.Tx) error {
		for _, q := range []string{`PRAGMA defer_foreign_keys=ON`, `DROP TABLE review_round_external_members`, `DROP TABLE review_round_external_credits`, `CREATE TEMP TABLE fixture_round_changes AS SELECT * FROM review_round_changes`, `DROP TABLE review_round_changes`, RoundSchemaV4SQL()[1], `INSERT INTO review_round_changes SELECT * FROM temp.fixture_round_changes`, `DROP TABLE temp.fixture_round_changes`} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
}

func setRoundTaper(t *testing.T, f *attemptFixture, pull int64, channel string, revision, taper int64, key string) []byte {
	t.Helper()
	b, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.writer, false), key, pull, channel, RoundControlRequest{ExpectedRevision: &revision, Action: "taper", Taper: &taper})
	if err != nil {
		t.Fatal(err)
	}
	var r RoundControlReceipt
	if json.Unmarshal(b, &r) != nil || r.Outcome != "taper_updated" || r.Revision != revision+1 {
		t.Fatal("invalid taper receipt")
	}
	return b
}

func TestRoundTaperCarriedProgressAndImmutableOrigin(t *testing.T) {
	for _, kind := range []string{"earned", "minimum", "old_latch", "closed"} {
		t.Run(kind, func(t *testing.T) {
			f := adoptionFixture(t)
			p := adoptionPrepared()
			switch kind {
			case "minimum":
				p.MinimumRemaining = 1
			case "old_latch":
				p.Taper = 1
				p.InitialDry = 1
				p.Tapered = true
			case "closed":
				p.Closed = true
			}
			adoptRound(t, f, p, "adopt")
			original := roundRead(t, f, 2, "cli")
			var first string
			if err := f.store.DB().QueryRow(`SELECT body_json FROM review_round_changes WHERE revision=1`).Scan(&first); err != nil {
				t.Fatal(err)
			}
			receipt := setRoundTaper(t, f, 2, "cli", 1, 2, "retune")
			c := roundRead(t, f, 2, "cli")
			want := original
			want.Revision = c.Revision
			want.ActorID = c.ActorID
			want.UpdatedAt = c.UpdatedAt
			want.Taper = 2
			want.Tapered = kind == "earned" || kind == "closed"
			if c != want {
				t.Fatalf("retune lost baseline or progress: got %+v want %+v", c, want)
			}
			adoptionValid(t, f)
			before := roundTables(t, f)
			replay := setRoundTaper(t, f, 2, "cli", 1, 2, "retune")
			if string(replay) != string(receipt) || before != roundTables(t, f) {
				t.Fatal("exact replay changed retained state")
			}
			var after string
			if err := f.store.DB().QueryRow(`SELECT body_json FROM review_round_changes WHERE revision=1`).Scan(&after); err != nil || after != first {
				t.Fatal("original adoption rewritten", err)
			}
			var grant int64
			if err := f.store.DB().QueryRow(`SELECT grant_revision FROM review_round_controls`).Scan(&grant); err != nil || grant != 1 {
				t.Fatal("retune replaced grant", err)
			}
		})
	}
}

func TestRoundTaperDoesNotSwallowReopenedOrPendingRound(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 8, "first")
	for i := 0; i < 2; i++ {
		a, id := roundCapture(t, f, i, false)
		roundFinalize(t, f, a, id, i, false)
	}
	roundGrant(t, f, 2, "cli", 1, 0, 3, "one-more")
	setRoundTaper(t, f, 2, "cli", 2, 1, "threshold")
	c := roundRead(t, f, 2, "cli")
	if c.Tapered || c.Used != 2 || c.Dry != 2 || c.Minimum != 2 || c.Maximum != 5 {
		t.Fatal("retune swallowed reopened round", c)
	}
	a, id := roundCapture(t, f, 2, false)
	setRoundTaper(t, f, 2, "cli", 3, 3, "pending-threshold")
	roundFinalize(t, f, a, id, 2, false)
	c = roundRead(t, f, 2, "cli")
	if !c.Tapered || c.Used != 3 || c.Dry != 3 || c.Epoch != 1 {
		t.Fatal("pending result lost original binding", c)
	}
	before := roundTables(t, f)
	roundCompleteHook(t, f, a.OperationID)
	if before != roundTables(t, f) {
		t.Fatal("pending completion counted twice")
	}
	adoptionValid(t, f)
}

func TestRoundTaperRejectsMalformedUnconfiguredAndAtomicFailure(t *testing.T) {
	f := roundFixture(t)
	zero, one, two := int64(0), int64(1), int64(2)
	_, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.writer, false), "absent", 2, "cli", RoundControlRequest{ExpectedRevision: &zero, Action: "taper", Taper: &two})
	attemptCode(t, err, "review_round_unconfigured")
	roundGrant(t, f, 2, "cli", 0, 0, 8, "initial")
	for _, r := range []RoundControlRequest{{ExpectedRevision: &one, Action: "taper"}, {ExpectedRevision: &one, Action: "taper", Taper: &zero}, {ExpectedRevision: &one, Action: "taper", Taper: &two, Minimum: &zero}, {ExpectedRevision: &one, Action: "close", Taper: &two}, {ExpectedRevision: &one, Action: "grant", Minimum: &zero, Maximum: &two, Taper: &two}} {
		if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.writer, false), "malformed", 2, "cli", r); err == nil {
			t.Fatal("malformed update accepted")
		}
	}
	before := roundTables(t, f)
	if _, err := f.store.DB().Exec(`CREATE TRIGGER fail_taper BEFORE INSERT ON review_round_changes WHEN NEW.action='taper' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.writer, false), "retry", 2, "cli", RoundControlRequest{ExpectedRevision: &one, Action: "taper", Taper: &one}); err == nil {
		t.Fatal("injected failure succeeded")
	}
	if before != roundTables(t, f) {
		t.Fatal("failed retune partially committed")
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER fail_taper`); err != nil {
		t.Fatal(err)
	}
	setRoundTaper(t, f, 2, "cli", 1, 1, "retry")
	adoptionValid(t, f)
}

func TestRoundTaperV4UpgradeAndFailurePreserveAuthority(t *testing.T) {
	f := adoptionFixture(t)
	adoptRound(t, f, adoptionPrepared(), "old-adoption")
	a, id := roundCapture(t, f, 0, false)
	roundFinalize(t, f, a, id, 0, false)
	retainRoundV4Changes(t, f)
	if _, err := f.store.DB().Exec(`UPDATE metadata SET value='4' WHERE key='schema:review_rounds'`); err != nil {
		t.Fatal(err)
	}
	adoptionValid(t, f)
	before := roundTables(t, f)
	if _, err := f.store.DB().Exec(`CREATE TRIGGER fail_round5 BEFORE UPDATE OF value ON metadata WHEN NEW.key='schema:review_rounds' AND NEW.value='5' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureRoundSchema(t.Context(), f.store); err == nil {
		t.Fatal("injected upgrade succeeded")
	}
	var v, ddl string
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_rounds'`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT sql FROM sqlite_schema WHERE name='review_round_changes'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if before != roundTables(t, f) || v != "4" || strings.TrimSuffix(ddl, ";") != strings.TrimSuffix(RoundSchemaV4SQL()[1], ";") {
		t.Fatal("failed migration changed retained schema or records")
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER fail_round5`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureRoundSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if before != roundTables(t, f) {
		t.Fatal("upgrade changed historical authority")
	}
	setRoundTaper(t, f, 2, "cli", 1, 2, "after-upgrade")
	adoptionValid(t, f)
}

func TestRoundTaperHistoryRejectsRewrittenLimitsOrOrigin(t *testing.T) {
	for _, field := range []string{"maximum", "initial_dry", "origin_key"} {
		t.Run(field, func(t *testing.T) {
			f := adoptionFixture(t)
			adoptRound(t, f, adoptionPrepared(), "adopt")
			setRoundTaper(t, f, 2, "cli", 1, 2, "retune")
			value := any(3)
			if field == "origin_key" {
				value = "different-origin"
			}
			if _, err := f.store.DB().Exec(`UPDATE review_round_changes SET body_json=json_set(body_json,?,?) WHERE revision=2`, "$.control."+field, value); err != nil {
				t.Fatal(err)
			}
			if err := ValidateRoundSnapshot(t.Context(), f.store.DB()); err == nil {
				t.Fatal("retune changed unrelated historical state", field)
			}
		})
	}
}

func TestRoundTaperSameValueRetainsHistoricalLatch(t *testing.T) {
	f := adoptionFixture(t)
	p := adoptionPrepared()
	p.Tapered = true
	p.InitialDry = 0
	adoptRound(t, f, p, "adopt")
	before := roundRead(t, f, 2, "cli")
	var changes, events int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_round_changes`).Scan(&changes); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	rev, taper := int64(1), int64(3)
	request := RoundControlRequest{ExpectedRevision: &rev, Action: "taper", Taper: &taper}
	b, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.writer, false), "unchanged", 2, "cli", request)
	if err != nil {
		t.Fatal(err)
	}
	var receipt RoundControlReceipt
	if json.Unmarshal(b, &receipt) != nil || receipt.Revision != 1 || receipt.Outcome != "taper_updated" || roundRead(t, f, 2, "cli") != before {
		t.Fatal("same threshold reinterpreted carried proof")
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_round_changes`) != changes || attemptCount(t, f.store, `SELECT count(*) FROM events`) != events {
		t.Fatal("no-op created setting history/event")
	}
	state := roundTables(t, f)
	replay, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.writer, false), "unchanged", 2, "cli", request)
	if err != nil || string(replay) != string(b) || roundTables(t, f) != state {
		t.Fatal("no-op replay changed state", err)
	}
	adoptionValid(t, f)
}

func TestRoundTaperHistoryRetainsOriginalAdoptionValidation(t *testing.T) {
	f := adoptionFixture(t)
	adoptRound(t, f, adoptionPrepared(), "adopt")
	setRoundTaper(t, f, 2, "cli", 1, 2, "retune")
	if _, err := f.store.DB().Exec(`UPDATE review_round_changes SET body_json=json_set(body_json,'$.initial_dry',3,'$.dry',3) WHERE revision=1`); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRoundSnapshot(t.Context(), f.store.DB()); err == nil {
		t.Fatal("corrupt original dry baseline accepted")
	}
}
