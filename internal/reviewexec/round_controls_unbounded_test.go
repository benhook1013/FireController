package reviewexec

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/roundadoptionmodel"
)

func unboundedPrepared() roundadoptionmodel.Prepared {
	p := adoptionPrepared()
	p.Version = 2
	p.MaximumUnbounded = true
	p.MaximumRemaining = 0
	p.MinimumRemaining = 2
	p.InitialDry = 0
	return p
}
func TestRoundUnboundedNativeMinimumTaperAndFiniteReplacement(t *testing.T) {
	f := adoptionFixture(t)
	p := unboundedPrepared()
	adoptRound(t, f, p, "unbounded")
	c := roundRead(t, f, 2, "cli")
	if !c.MaximumUnbounded || c.Maximum != 0 || roundDone(c) || checkRoundOpen(c) != nil {
		t.Fatal("unbounded placeholder treated as exhausted", c)
	}
	for n := 0; n < 3; n++ {
		a, id := roundCapture(t, f, n, false)
		roundFinalize(t, f, a, id, n, false)
		c = roundRead(t, f, 2, "cli")
		if c.Used != int64(n+1) || c.Tapered != (n == 2) {
			t.Fatal("minimum/taper lost", c)
		}
		adoptionValid(t, f)
	}
	progress, err := f.e.ReadRoundProgress(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), []int64{2})
	if err != nil || !progress.Items[0].Channels[1].MaximumUnbounded || progress.Items[0].Channels[1].Maximum != 0 {
		t.Fatal("progress lost no-maximum fact", err)
	}
	one := int64(1)
	if _, err = f.e.ConfigureRoundControl(t.Context(), adoptionScope(t, f, f.admin, true), "close-unbounded", 2, "cli", RoundControlRequest{ExpectedRevision: &one, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	if !roundRead(t, f, 2, "cli").MaximumUnbounded {
		t.Fatal("close erased original bound semantics")
	}
	adoptionValid(t, f)
	roundGrant(t, f, 2, "cli", 2, 0, 7, "finite-replacement")
	c = roundRead(t, f, 2, "cli")
	if c.MaximumUnbounded || c.Maximum != 10 || c.Used != 3 || c.Minimum != 3 || c.OriginSource != p.Source || c.Dry != 3 || c.Tapered || c.Epoch != 1 {
		t.Fatal("finite limits lost cumulative imported progress", c)
	}
	adoptionValid(t, f)
}
func TestRoundUnboundedVersionsStrictPlaceholderAndSafeNumbers(t *testing.T) {
	p := unboundedPrepared()
	p.MinimumRemaining = roundSafeInteger
	if _, _, err := normalizeRoundAdoption(p); err != nil {
		t.Fatal("safe minimum-only policy refused", err)
	}
	for _, change := range []func(*roundadoptionmodel.Prepared){
		func(p *roundadoptionmodel.Prepared) { p.Version = 1 },
		func(p *roundadoptionmodel.Prepared) { p.MaximumRemaining = 1 },
		func(p *roundadoptionmodel.Prepared) { p.MaximumUnbounded = false },
		func(p *roundadoptionmodel.Prepared) { p.MinimumRemaining = roundSafeInteger + 1 },
	} {
		bad := p
		change(&bad)
		if _, _, err := normalizeRoundAdoption(bad); err == nil {
			t.Fatal("accepted ambiguous/inexact bound", bad)
		}
	}
	legacy := adoptionPrepared()
	body, _ := json.Marshal(legacy)
	if strings.Contains(string(body), "maximum_unbounded") {
		t.Fatal("v1 canonical bytes changed")
	}
	if _, err := decodeRoundAdoption(body); err != nil {
		t.Fatal(err)
	}
	explicitFalse := append(append([]byte{}, body[:len(body)-1]...), []byte(`,"maximum_unbounded":false}`)...)
	if _, err := decodeRoundAdoption(explicitFalse); err == nil {
		t.Fatal("v1 accepted new field")
	}
	p = unboundedPrepared()
	raw, _ := json.Marshal(p)
	if _, err := decodeRoundAdoption(raw); err != nil {
		t.Fatal("v2 explicit flag refused", err)
	}
	for _, replacement := range []string{`null`, `"true"`, `1`} {
		rawBad := strings.Replace(string(raw), `"maximum_unbounded":true`, `"maximum_unbounded":`+replacement, 1)
		if _, err := decodeRoundAdoption([]byte(rawBad)); err == nil {
			t.Fatal("ambiguous flag accepted")
		}
	}
}
func TestRoundUnboundedIntegerCapacityAndPhysicalChecks(t *testing.T) {
	f := adoptionFixture(t)
	p := unboundedPrepared()
	p.MinimumRemaining = roundSafeInteger
	adoptRound(t, f, p, "safe-bound")
	c := roundRead(t, f, 2, "cli")
	c.Used = roundSafeInteger - 1
	if !validRoundControl(c) || !roundHasCapacity(c, 0) || roundHasCapacity(c, 1) {
		t.Fatal("counter reservation boundary incorrect")
	}
	c.Used = roundSafeInteger
	if !validRoundControl(c) || checkRoundOpen(c) == nil || roundHasCapacity(c, 0) {
		t.Fatal("safe bound can overflow")
	}
	c.Used++
	if validRoundControl(c) {
		t.Fatal("noninteroperable count accepted")
	}
	// A real eligible completion proves the new ordinal constraint independently
	// of the stricter retained-graph validator. Never create synthetic credit.
	a, id := roundCapture(t, f, 0, false)
	roundFinalize(t, f, a, id, 0, false)
	if _, err := f.store.DB().Exec(`UPDATE review_round_completions SET ordinal=?`, roundSafeInteger); err != nil {
		t.Fatal("SQL retains obsolete 1000 cap", err)
	}
	if err := ValidateRoundSnapshot(t.Context(), f.store.DB()); err == nil {
		t.Fatal("ordinal gap accepted as native credit")
	}
	if _, err := f.store.DB().Exec(`UPDATE review_round_completions SET ordinal=?`, roundSafeInteger+1); err == nil {
		t.Fatal("SQL accepted unsafe ordinal")
	}
	if _, err := f.store.DB().Exec(`UPDATE review_round_completions SET ordinal=1`); err != nil {
		t.Fatal(err)
	}
	adoptionValid(t, f)
	for _, statement := range []string{`UPDATE review_round_controls SET maximum=1`, `UPDATE review_round_controls SET maximum_unbounded=0`, `UPDATE review_round_controls SET used=9007199254740992`} {
		if _, err := f.store.DB().Exec(statement); err == nil {
			t.Fatal("invalid unbounded SQL state accepted", statement)
		}
	}
	adoptionValid(t, f)
}
func TestRoundV2MigrationPreservesFiniteHistoryAndPredecessorDeclarations(t *testing.T) {
	f := adoptionFixture(t)
	adoptRound(t, f, adoptionPrepared(), "finite-v2")
	a, id := roundCapture(t, f, 0, false)
	roundFinalize(t, f, a, id, 0, false)
	original := roundRead(t, f, 2, "cli")
	var body string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_round_changes`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	statements := []string{`ALTER TABLE review_round_controls RENAME TO fixture_v3_controls`, RoundSchemaV2SQL()[0], `INSERT INTO review_round_controls(` + roundColumnsV2 + `) SELECT ` + roundColumnsV2 + ` FROM fixture_v3_controls`, `DROP TABLE fixture_v3_controls`, `ALTER TABLE review_round_completions RENAME TO fixture_v3_completions`, RoundSchemaV2SQL()[4], `INSERT INTO review_round_completions SELECT * FROM fixture_v3_completions`, `DROP TABLE fixture_v3_completions`, `UPDATE metadata SET value='2' WHERE key='schema:review_rounds'`}
	for _, s := range statements {
		if _, err := f.store.DB().Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	adoptionValid(t, f)
	if err := EnsureRoundSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if after := roundRead(t, f, 2, "cli"); after != original {
		t.Fatal("migration rewrote finite policy/progress", after)
	}
	var afterBody string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_round_changes`).Scan(&afterBody); err != nil || afterBody != body {
		t.Fatal("migration rewrote canonical history", err)
	}
	adoptionValid(t, f)
}
