package reviewexec

import (
	"reflect"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestHostedAllowanceRetainedCooldownSecondsBoundaries(t *testing.T) {
	response := time.Date(2026, 10, 31, 1, 2, 3, 123456789, time.UTC)
	for _, test := range []struct {
		name  string
		delay time.Duration
		valid bool
	}{
		{"one second", time.Second, false},
		{"minimum rounded seconds", 2 * time.Second, true},
		{"short seconds", 13 * time.Second, true},
		{"under minute", 59 * time.Second, true},
		{"one minute from seconds", time.Minute, true},
		{"between minutes", 61 * time.Second, true},
		{"original rounded minute minimum", 2 * time.Minute, true},
		{"original rounded minutes", 3 * time.Minute, true},
		{"maximum rounded duration", 24 * time.Hour, true},
		{"over maximum", 24*time.Hour + time.Second, false},
		{"fractional seconds", 2*time.Second + time.Nanosecond, false},
		{"negative", -time.Second, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			observation := reviewprovider.HostedAllowanceObservation{State: "cooldown_observed", Reason: "provider_cooldown_observed", ResponseCreatedAt: response, RetryNotBefore: response.Add(test.delay)}
			if got := validHostedAllowanceOutcome(observation); got != test.valid {
				t.Fatal("retained cooldown bound", test.delay, got)
			}
		})
	}
}

func TestHostedAllowanceSecondsRetainScheduleRestartAndRejectMalformed(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "retained seconds", true: "fractional deadline"}[invalid], func(t *testing.T) {
			f, a, in := cooldownObservationFixture(t)
			in.Observation.RetryNotBefore = in.Observation.ResponseCreatedAt.Add(13 * time.Second)
			if invalid {
				in.Observation.RetryNotBefore = in.Observation.RetryNotBefore.Add(time.Nanosecond)
			}
			before := hostedAllowanceState(t, f, a)
			err := f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, 1, in)
			if invalid {
				attemptCode(t, err, "validation_failed")
				if before != hostedAllowanceState(t, f, a) {
					t.Fatal("malformed seconds changed retained journal")
				}
				return
			}
			if err != nil {
				t.Fatal("retain seconds", err)
			}
			if _, err := f.e.ScheduleHostedCooldown(t.Context(), attemptProject, a.ID, 1); err != nil {
				t.Fatal("schedule seconds", err)
			}
			original := bindingOperation(t, f, f.scope(t, attemptProject, f.writer, false), a.OperationID)
			if original.Wait == nil || original.Wait.RetryNotBefore != in.Observation.RetryNotBefore.Format(time.RFC3339Nano) {
				t.Fatal("seconds deadline lost", original)
			}
			if err := ValidateAllowanceSnapshot(t.Context(), f.store.DB(), 9); err != nil {
				t.Fatal("retained seconds snapshot", err)
			}
			cliAllowanceReopen(t, f)
			after := bindingOperation(t, f, f.scope(t, attemptProject, f.writer, false), a.OperationID)
			if !reflect.DeepEqual(original, after) {
				t.Fatal("restart changed seconds intent", after)
			}
			if err := f.e.RecordHostedCooldown(t.Context(), attemptProject, a.ID, 1, in); err != nil {
				t.Fatal("seconds replay", err)
			}
		})
	}
}
