package reviewexec

import (
	"strings"
	"testing"
	"time"
)

func TestAutomaticIntentSharedWaitAvoidsDiscoveryAndExpires(t *testing.T) {
	for _, kind := range []string{"source", "cli", "occupied"} {
		t.Run(kind, func(t *testing.T) {
			f := schedulingFixture(t)
			now := time.Now().UTC().Add(time.Second)
			f.e.schedulingNow = func() time.Time { return now }
			f.e.attemptNow = func() time.Time { return now }
			f.e.sourceReadGroups = map[string]string{"test-capability": strings.Repeat("a", 64)}
			r := f.enqueue(t, attemptProject, "shared-wait", "cli", 2, 1)
			a := f.start(t, attemptProject, r)
			var due time.Time
			switch kind {
			case "source":
				ok, err := f.e.ScheduleSourceRetry(t.Context(), attemptProject, a.ID, a.Revision, sourceFailure(now))
				if err != nil || !ok {
					t.Fatal(ok, err)
				}
				due = now.Add(time.Minute)
			case "cli":
				in := CLIAllowanceObservation{Anchor: attemptAnchor(attemptProject, 2), ObservedAt: now.Format(time.RFC3339Nano), Remaining: 0, Limit: 3, FullCapacityInMS: 86400000, RollingWindowMS: 86400000, ChangedFiles: 2}
				if err := f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, a.Revision, in); err != nil {
					t.Fatal(err)
				}
				if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
					t.Fatal(err)
				}
				due = cliWaitDue(t, cliWaitRead(t, f, r.ID))
			}
			schedulingCLIEnable(t, f, 0, true)
			reader := &refreshTestReader{}
			readers := map[string]TargetReader{"test-capability": reader}
			for i := 0; i < 2; i++ {
				worked, err := f.e.generateAutomaticCLI(t.Context(), readers)
				if err != nil || worked {
					t.Fatal("discovery during shared hold", worked, err)
				}
				worked, err = f.e.runNext(t.Context(), readers)
				if err != nil || worked || reader.calls.Load() != 0 {
					t.Fatal("source reads during shared hold", worked, err, reader.calls.Load())
				}
			}
			if kind == "occupied" {
				if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", "test_released", ""); err != nil {
					t.Fatal(err)
				}
			} else {
				now = due.Add(time.Millisecond)
			}
			worked, err := f.e.generateAutomaticCLI(t.Context(), readers)
			if err != nil || !worked {
				t.Fatal("expired shared hold suppressed discovery", worked, err)
			}
			worked, err = f.e.runNext(t.Context(), readers)
			if err != nil || !worked || reader.calls.Load() == 0 {
				t.Fatal("discovery did not resume", worked, err, reader.calls.Load())
			}
		})
	}
}

func TestAutomaticIntentHostedPRCooldownDoesNotBlockLaterDiscovery(t *testing.T) {
	f := schedulingFixture(t)
	r := f.enqueue(t, attemptProject, "front-local-cooldown", "hosted", 2, 1)
	priorityCooldown(t, f, r, 2, time.Hour)
	schedulingEnable(t, f, 0, true)
	reader := &refreshTestReader{}
	readers := map[string]TargetReader{"test-capability": reader}
	worked, err := f.e.generateAutomaticHosted(t.Context(), readers)
	if err != nil || !worked {
		t.Fatal("PR-local wait blocked discovery", worked, err)
	}
	worked, err = f.e.runNext(t.Context(), readers)
	if err != nil || !worked || reader.calls.Load() == 0 {
		t.Fatal("later source not inspected", worked, err)
	}
	selected := schedulingLatest(t, f)
	if selected.State != "selected" || cliWaitRead(t, f, selected.OperationID).PullNumber != 4 {
		t.Fatal("later eligible PR not selected", selected)
	}
}
