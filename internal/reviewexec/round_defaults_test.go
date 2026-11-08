package reviewexec

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/strictjson"
)

func TestProjectRoundDefaultsInitializeOnlyAbsentControls(t *testing.T) {
	f := newAttemptFixtureWithPolicy(t, false)
	scope := f.scope(t, attemptProject, f.admin, true)
	input := policyInput(0)
	input.RoundDefaults = &RoundDefaults{Hosted: RoundBounds{Minimum: 2, Maximum: 5}, CLI: RoundBounds{Minimum: 0, Maximum: 9}}
	first := policyConfigure(t, f, scope, "project-plan", input)
	cloned := clonePolicyConfiguration(input)
	input.RoundDefaults.Hosted.Maximum = 99
	if cloned.RoundDefaults.Hosted.Maximum != 5 || policyRead(t, f, attemptProject, f.reader).Policy.RoundDefaults.Hosted.Maximum != 5 {
		t.Fatal("policy shares mutable defaults")
	}
	if _, err := f.e.Configure(t.Context(), scope, "enroll-original", enrollmentConfiguration(0, []int64{2}, []string{"hosted", "cli"})); err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{"hosted", "cli"} {
		actual := roundRead(t, f, 2, channel)
		want := cloned.RoundDefaults.Hosted
		if channel == "cli" {
			want = cloned.RoundDefaults.CLI
		}
		if actual.Minimum != want.Minimum || actual.Maximum != want.Maximum || actual.Taper != roundTaper(channel) || actual.Used != 0 || actual.Dry != 0 || actual.Closed {
			t.Fatal("wrong project enrollment defaults", actual)
		}
	}
	one := int64(1)
	if _, err := f.e.ConfigureRoundControl(t.Context(), scope, "close-existing", 2, "hosted", RoundControlRequest{ExpectedRevision: &one, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	closed := roundRead(t, f, 2, "hosted")
	cli := roundRead(t, f, 2, "cli")
	next := policyInput(1)
	next.RoundDefaults = &RoundDefaults{Hosted: RoundBounds{Minimum: 1, Maximum: 7}, CLI: RoundBounds{Minimum: 2, Maximum: 8}}
	policyConfigure(t, f, scope, "changed-plan", next)
	if _, err := f.e.Configure(t.Context(), scope, "enroll-next", enrollmentConfiguration(1, []int64{4, 2}, []string{"hosted", "cli"})); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(closed, roundRead(t, f, 2, "hosted")) || !reflect.DeepEqual(cli, roundRead(t, f, 2, "cli")) || roundRead(t, f, 4, "hosted").Maximum != 7 || roundRead(t, f, 4, "cli").Maximum != 8 {
		t.Fatal("project defaults rewrote existing controls or missed new controls")
	}
	// An unrelated project with no policy still enrolls using product defaults.
	other := f.scope(t, otherAttemptProject, f.admin, true)
	if _, err := f.e.Configure(t.Context(), other, "other-default-plan", enrollmentConfiguration(0, []int64{2}, []string{"hosted", "cli"})); err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{"hosted", "cli"} {
		actual, err := f.e.ReadRoundControl(t.Context(), evidenceReadScope(t, f, otherAttemptProject, f.other), 2, channel)
		maximum := int64(6)
		if channel == "hosted" {
			maximum = 3
		}
		if err != nil || actual.Minimum != 2 || actual.Maximum != maximum || actual.Taper != roundTaper(channel) {
			t.Fatal("project policy leaked or unconfigured fallback failed", actual, err)
		}
	}
	before := enrollmentState(t, f)
	replay, err := f.e.ConfigurePolicy(t.Context(), scope, "project-plan", cloned)
	if err != nil || string(replay) != string(first) || before != enrollmentState(t, f) {
		t.Fatal("old policy receipt replay changed later controls", err)
	}
	if err := ValidateRoundSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"review_operations", "review_execution_attempts", "review_execution_reservations", "review_round_completions"} {
		if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != 0 {
			t.Fatal("policy/enrollment performed review work", table)
		}
	}
}

func TestProjectRoundDefaultsStrictShapeBoundsAndOmission(t *testing.T) {
	legacy := Policy{RequiredChannels: []string{"hosted", "cli"}, PausedChannels: []string{}}
	if got := encoded(legacy); got != `{"required_channels":["hosted","cli"],"paused_channels":[],"max_review_files":0}` {
		t.Fatal("absent defaults changed canonical legacy policy", got)
	}
	for _, body := range []string{
		`null`, `{}`, `{"hosted":{"minimum":0,"maximum":1}}`,
		`{"hosted":{"maximum":1},"cli":{"minimum":0,"maximum":1}}`,
		`{"hosted":{"minimum":null,"maximum":1},"cli":{"minimum":0,"maximum":1}}`,
		`{"hosted":{"minimum":0,"maximum":1,"taper":1},"cli":{"minimum":0,"maximum":1}}`,
		`{"hosted":{"minimum":0,"maximum":1},"cli":null}`,
		`{"hosted":{"minimum":0,"maximum":1},"cli":{"minimum":0,"maximum":1},"manual":{}}`,
	} {
		var p Policy
		if strictjson.Decode([]byte(`{"required_channels":["cli"],"max_review_files":0,"round_defaults":`+body+`}`), &p) == nil {
			t.Fatal("accepted malformed optional defaults", body)
		}
	}
	for _, bounds := range []RoundBounds{{-1, 1}, {0, 0}, {0, 1001}, {2, 1}} {
		p := legacy
		p.RoundDefaults = &RoundDefaults{Hosted: bounds, CLI: RoundBounds{0, 1}}
		if validPolicy(p) {
			t.Fatal("accepted invalid defaults", bounds)
		}
	}
	for _, bounds := range []RoundBounds{{0, 1}, {1000, 1000}, {2, 5}} {
		p := legacy
		p.RoundDefaults = &RoundDefaults{Hosted: bounds, CLI: bounds}
		body, _ := json.Marshal(p)
		var roundtrip Policy
		if len(body) > 1024 || strictjson.Decode(body, &roundtrip) != nil || !validPolicy(roundtrip) || !reflect.DeepEqual(roundtrip, p) || !strings.Contains(string(body), "round_defaults") {
			t.Fatal("valid bounds failed policy roundtrip", string(body))
		}
	}
}

func TestProjectRoundDefaultsHTTPPreservesPausedChannels(t *testing.T) {
	f := newAttemptFixture(t)
	path := "/api/v1/projects/" + attemptProject + "/reviews/policy"
	body := `{"expected_revision":1,"required_channels":["cli"],"paused_channels":["hosted"],"max_review_files":42,"round_defaults":{"hosted":{"minimum":0,"maximum":4},"cli":{"minimum":3,"maximum":7}}}`
	response := f.request("POST", path, f.admin, "policy-round-defaults", body)
	evidenceHTTPRequire(t, response, 200, "")
	stored := policyRead(t, f, attemptProject, f.reader).Policy
	if stored.RoundDefaults == nil || stored.RoundDefaults.CLI.Minimum != 3 || stored.RoundDefaults.Hosted.Maximum != 4 || len(stored.PausedChannels) != 1 || stored.PausedChannels[0] != "hosted" || stored.MaxReviewFiles != 42 {
		t.Fatal("HTTP lost existing policy or round defaults", stored)
	}
	malformed := strings.Replace(body, `"expected_revision":1`, `"expected_revision":2`, 1)
	malformed = strings.Replace(malformed, `"minimum":3`, `"minimum":null`, 1)
	evidenceHTTPRequire(t, f.request("POST", path, f.admin, "policy-invalid-defaults", malformed), 400, "")
	if policyRead(t, f, attemptProject, f.reader).Revision != 2 {
		t.Fatal("malformed defaults advanced policy")
	}
}
