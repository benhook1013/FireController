package reviewexec

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestRoundGrantReopensTaperWithoutChangingBoundsOrEarnedStreak(t *testing.T) {
	for _, tc := range []struct {
		name     string
		minimum  int64
		accepted bool
	}{
		{"max_only_clean", 0, false},
		{"max_only_findings", 0, true},
		{"minimum_two_clean", 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := roundFixture(t)
			roundGrant(t, f, 2, "cli", 0, 0, 8, "initial")
			for i := 0; i < 3; i++ {
				a, evidence := roundCapture(t, f, i, false)
				roundFinalize(t, f, a, evidence, i, false)
			}
			before := roundRead(t, f, 2, "cli")
			if !before.Tapered {
				t.Fatal("fixture has not tapered")
			}
			path := "/api/v1/projects/" + attemptProject + "/reviews/rounds/2/cli"
			body := fmt.Sprintf(`{"expected_revision":1,"action":"grant","minimum":%d,"maximum":2}`, tc.minimum)
			grant := f.request("POST", path, f.writer, "extra-rounds", body)
			evidenceHTTPRequire(t, grant, 200, "")
			opened := roundRead(t, f, 2, "cli")
			if opened.Tapered || opened.Closed || opened.Dry != 3 || opened.Used != 3 || opened.Minimum != 3+tc.minimum || opened.Maximum != 5 || opened.Epoch != before.Epoch {
				t.Fatal("grant failed to reopen with original history and exact bounds", opened)
			}
			adoptionValid(t, f)
			if original, err := readRoundGrant(t.Context(), f.store.DB(), attemptProject, 2, "cli", 2, 1); err != nil || original.Tapered {
				t.Fatal("reopened grant cannot bind a new request", original, err)
			}
			a, evidence := roundCapture(t, f, 3, tc.accepted)
			if current := roundRead(t, f, 2, "cli"); current.Tapered || current.Used != 3 {
				t.Fatal("unfinalized result received early taper credit", current)
			}
			// A real database restart preserves the reopening boundary and the
			// original in-flight binding without another grant or provider call.
			var sequence int
			var databaseName, databasePath string
			if err := f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&sequence, &databaseName, &databasePath); err != nil {
				t.Fatal(err)
			}
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := core.Open(t.Context(), filepath.Dir(databasePath))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			f.store = reopened
			f.e, err = New(reopened, attemptCapabilities)
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			f.e.Register(mux)
			f.handler = mux
			if current := roundRead(t, f, 2, "cli"); current != opened {
				t.Fatal("restart changed reopened progress", current)
			}
			adoptionValid(t, f)
			roundFinalize(t, f, a, evidence, 3, tc.accepted)
			current := roundRead(t, f, 2, "cli")
			wantDry := int64(4)
			if tc.accepted {
				wantDry = 0
			}
			if current.Used != 4 || current.Dry != wantDry || current.Tapered != (tc.minimum == 0 && !tc.accepted) || current.Minimum != opened.Minimum || current.Maximum != 5 {
				t.Fatal("new completion failed preserved taper/minimum accounting", current)
			}
			if tc.minimum == 2 {
				a, evidence = roundCapture(t, f, 4, false)
				roundFinalize(t, f, a, evidence, 4, false)
				if current = roundRead(t, f, 2, "cli"); current.Used != 5 || !current.Tapered || !roundDone(current) {
					t.Fatal("minimum two did not require exactly two additional clean completions", current)
				}
			}
			// Exact-key replay is a receipt, even after this grant tapers again.
			state := roundTables(t, f)
			replay := f.request("POST", path, f.writer, "extra-rounds", body)
			if replay.Code != 200 || replay.Body.String() != grant.Body.String() || roundTables(t, f) != state {
				t.Fatal("grant replay reopened again or changed accounting")
			}
			adoptionValid(t, f)
		})
	}
}

func TestRoundGrantReopeningIncludesAlreadyCapturedRound(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 8, "initial")
	late, evidence := roundCapture(t, f, 0, false)
	for i := 1; i <= 3; i++ {
		a, id := roundCapture(t, f, i, false)
		roundFinalize(t, f, a, id, i, false)
	}
	if !roundRead(t, f, 2, "cli").Tapered {
		t.Fatal("fixture has not tapered")
	}
	roundGrant(t, f, 2, "cli", 1, 0, 1, "one-more-includes-occupied")
	current := roundRead(t, f, 2, "cli")
	if current.Tapered || current.Used != 3 || current.Minimum != 3 || current.Maximum != 4 || current.Dry != 3 {
		t.Fatal("reopening changed one-more bounds or streak", current)
	}
	if pending := readProgress(t, f, 2).Items[0].Channels[1].Pending; pending != 1 {
		t.Fatal("reopening lost original occupied round", pending)
	}
	roundFinalize(t, f, late, evidence, 0, false)
	current = roundRead(t, f, 2, "cli")
	if current.Used != 4 || current.Dry != 4 || !current.Tapered || !roundDone(current) {
		t.Fatal("original occupied round did not satisfy reopening exactly once", current)
	}
	state := roundTables(t, f)
	roundCompleteHook(t, f, late.OperationID)
	if roundTables(t, f) != state {
		t.Fatal("original completion replay counted twice")
	}
	adoptionValid(t, f)
}

func TestRoundGrantReopensImportedHostedTaperWithoutReplacingHistory(t *testing.T) {
	f := adoptionFixture(t)
	p := adoptionPrepared()
	p.Channel, p.InitialDry, p.Taper, p.Tapered = "hosted", 1, 1, true
	adoptRound(t, f, p, "hosted-taper")
	before := roundRead(t, f, 2, "hosted")
	if !before.Tapered {
		t.Fatal("fixture lacks carried hosted taper")
	}
	roundGrant(t, f, 2, "hosted", 1, 0, 2, "two-more-hosted")
	current := roundRead(t, f, 2, "hosted")
	if current.Tapered || current.Minimum != 0 || current.Maximum != 2 || current.Used != 0 || current.Dry != 1 || !sameRoundBaseline(before, current) {
		t.Fatal("hosted reopening erased carry or changed requested bounds", current)
	}
	if _, err := readRoundGrant(t.Context(), f.store.DB(), attemptProject, 2, "hosted", 2, 1); err != nil {
		t.Fatal("hosted reopened grant unavailable", err)
	}
	adoptionValid(t, f)
}
