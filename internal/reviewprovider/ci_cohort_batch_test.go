package reviewprovider

import (
	"context"
	"testing"
)

func TestCICohortPreparedBatchExactConsumption(t *testing.T) {
	g, targets, calls := fullCountCohortFixture(t, "")
	c, err := g.NewCIGateCohort(t.Context(), targets, "main")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.PrepareBatched(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 40 {
		t.Fatal(calls.Load())
	}
	if err = c.PrepareBatched(t.Context()); err == nil {
		t.Fatal("reprepared batch")
	}
	late := targets[0]
	late.PullNumber = 9
	if _, err = c.ObserveCIGate(t.Context(), late, "main"); err == nil {
		t.Fatal("late target admitted")
	}
	for _, target := range targets {
		o, err := c.ObserveCIGate(t.Context(), target, "main")
		if err != nil || o.State != "satisfied" || o.Target != target {
			t.Fatal(o, err)
		}
		if _, err = c.ObserveCIGate(t.Context(), target, "main"); err == nil {
			t.Fatal("repeat consumption")
		}
	}
	if calls.Load() != 40 {
		t.Fatal("cached consumption fetched", calls.Load())
	}
}

func TestCICohortPreparedBatchCloseAndCallerCancel(t *testing.T) {
	for _, closeCohort := range []bool{false, true} {
		g, targets, calls := fullCountCohortFixture(t, "")
		c, err := g.NewCIGateCohort(t.Context(), targets, "main")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err = c.PrepareBatched(t.Context()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		if closeCohort {
			c.Close()
		} else {
			cancel()
		}
		o, err := c.ObserveCIGate(ctx, targets[0], "main")
		cancel()
		if err == nil || o.State == "satisfied" || calls.Load() != 40 {
			t.Fatal(o, err, calls.Load())
		}
	}
}

func TestCICohortPreparedBatchRefusesAfterConsumption(t *testing.T) {
	g, targets, _ := fullCountCohortFixture(t, "")
	c, err := g.NewCIGateCohort(t.Context(), targets, "main")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.ObserveCIGate(t.Context(), targets[0], "main"); err != nil {
		t.Fatal(err)
	}
	if err = c.PrepareBatched(t.Context()); err == nil {
		t.Fatal("batch replaced partial collection")
	}
}
