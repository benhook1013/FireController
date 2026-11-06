package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
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
		for _, q := range []string{`PRAGMA defer_foreign_keys=ON`, `CREATE TEMP TABLE fixture_round_changes AS SELECT * FROM review_round_changes`, `DROP TABLE review_round_changes`, RoundSchemaV4SQL()[1], `INSERT INTO review_round_changes SELECT * FROM temp.fixture_round_changes`, `DROP TABLE temp.fixture_round_changes`} {
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
	roundSnapshotValid(t, f)
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
	roundSnapshotValid(t, f)
}

func TestRoundTaperNativeClosedHistoryPreservesAllowanceAndSameValue(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 8, "initial-native")
	a, id := roundCapture(t, f, 0, false)
	roundFinalize(t, f, a, id, 0, false)
	one := int64(1)
	if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.writer, false), "close-native", 2, "cli", RoundControlRequest{ExpectedRevision: &one, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	before := roundRead(t, f, 2, "cli")
	setRoundTaper(t, f, 2, "cli", 2, 1, "lower-native")
	after := roundRead(t, f, 2, "cli")
	if !after.Closed || !after.Tapered || after.Used != before.Used || after.Dry != before.Dry || after.Minimum != before.Minimum || after.Maximum != before.Maximum || after.Epoch != before.Epoch {
		t.Fatal("threshold altered native grant or counters", before, after)
	}
	three := int64(3)
	history := attemptCount(t, f.store, `SELECT count(*) FROM review_round_changes`)
	if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.writer, false), "same-native", 2, "cli", RoundControlRequest{ExpectedRevision: &three, Action: "taper", Taper: &one}); err != nil {
		t.Fatal(err)
	}
	if actual := roundRead(t, f, 2, "cli"); actual != after || attemptCount(t, f.store, `SELECT count(*) FROM review_round_changes`) != history {
		t.Fatal("same threshold changed control history", actual)
	}
	setRoundTaper(t, f, 2, "cli", 3, 3, "raise-native")
	raised := roundRead(t, f, 2, "cli")
	if !raised.Closed || raised.Tapered || raised.Minimum != before.Minimum || raised.Maximum != before.Maximum || raised.Used != before.Used || raised.Dry != before.Dry {
		t.Fatal("raise reopened closure or changed allowance", raised)
	}
	roundSnapshotValid(t, f)
}

func TestRoundTaperNativeHistoryRejectsAlteredAllowance(t *testing.T) {
	for _, field := range []string{"minimum", "maximum"} {
		t.Run(field, func(t *testing.T) {
			f := roundFixture(t)
			roundGrant(t, f, 2, "cli", 0, 0, 8, "initial-native")
			setRoundTaper(t, f, 2, "cli", 1, 1, "threshold-native")
			if _, err := f.store.DB().Exec(`UPDATE review_round_changes SET body_json=json_set(body_json,'$.control.` + field + `',3) WHERE action='taper'`); err != nil {
				t.Fatal(err)
			}
			if err := ValidateRoundSnapshot(t.Context(), f.store.DB()); err == nil {
				t.Fatal("threshold rewrite accepted", field)
			}
		})
	}
}

func TestRoundNativeRejectsUnsupportedCarriedBaseline(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 8, "native")
	c := roundRead(t, f, 2, "cli")
	for _, change := range []func(*RoundControl){
		func(v *RoundControl) { v.OriginSource = "unsupported" },
		func(v *RoundControl) { v.InitialDry = 1 },
		func(v *RoundControl) { v.InitiallyTapered = true },
		func(v *RoundControl) { v.MaximumUnbounded = true; v.Maximum = 0 },
	} {
		changed := c
		change(&changed)
		if validRoundControl(changed) {
			t.Fatal("unverified carried baseline accepted", changed)
		}
	}
}
