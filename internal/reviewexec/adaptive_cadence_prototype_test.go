package reviewexec

import (
	"testing"
	"time"
)

// Test-only policy prototype: deliberately unreachable from serving code.
// Unknown process-local demand must never be inferred absent from DB/queue reads.
type cadencePrototypeInput struct {
	demandKnown, unchanged, terminal, healthy                  bool
	watch, providerWork, pendingIntent, dirtyHint, sourceMoved bool
	stablePasses                                               int
}

func cadencePrototypeDelay(in cadencePrototypeInput) time.Duration {
	if !in.demandKnown || !in.unchanged || !in.terminal || !in.healthy || in.watch || in.providerWork || in.pendingIntent || in.dirtyHint || in.sourceMoved {
		return 5 * time.Minute
	}
	if in.stablePasses >= 3 {
		return 15 * time.Minute
	}
	if in.stablePasses >= 2 {
		return 10 * time.Minute
	}
	return 5 * time.Minute
}
func TestAdaptiveCadencePrototypeDemandResets(t *testing.T) {
	cold := cadencePrototypeInput{demandKnown: true, unchanged: true, terminal: true, healthy: true, stablePasses: 3}
	if cadencePrototypeDelay(cold) != 15*time.Minute {
		t.Fatal("cold delay")
	}
	cases := map[string]func(*cadencePrototypeInput){
		"unknown demand":                         func(x *cadencePrototypeInput) { x.demandKnown = false },
		"changed comparison":                     func(x *cadencePrototypeInput) { x.unchanged = false },
		"running or unknown CI":                  func(x *cadencePrototypeInput) { x.terminal = false },
		"error":                                  func(x *cadencePrototypeInput) { x.healthy = false },
		"active watcher":                         func(x *cadencePrototypeInput) { x.watch = true },
		"provider work":                          func(x *cadencePrototypeInput) { x.providerWork = true },
		"pending intent":                         func(x *cadencePrototypeInput) { x.pendingIntent = true },
		"in-flight or lost acknowledgement hint": func(x *cadencePrototypeInput) { x.dirtyHint = true },
		"head base policy movement":              func(x *cadencePrototypeInput) { x.sourceMoved = true },
		"restart":                                func(x *cadencePrototypeInput) { x.stablePasses = 0 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			x := cold
			change(&x)
			if cadencePrototypeDelay(x) != 5*time.Minute {
				t.Fatal("demand slowed")
			}
		})
	}
}
func TestAdaptiveCadencePrototypeBoundedSavings(t *testing.T) {
	// Synthetic 24-target scenario, NOT the measured live cold-target count.
	// Count steady-state periodic opportunities only; no provider I/O or dispatch.
	const targets = 24
	for coldCount := 0; coldCount <= targets; coldCount++ {
		baseline, candidate := 0, 0
		for target := 0; target < targets; target++ {
			in := cadencePrototypeInput{demandKnown: true, unchanged: true, terminal: true, healthy: true, stablePasses: 3, watch: target >= coldCount}
			baseline += int(time.Hour / (5 * time.Minute))
			candidate += int(time.Hour / cadencePrototypeDelay(in))
		}
		if baseline != 288 || candidate != 288-8*coldCount {
			t.Fatalf("cold=%d base=%d candidate=%d", coldCount, baseline, candidate)
		}
	}
	// Backoff must be earned by successful stable passes and reset by demand.
	for n, want := range []time.Duration{5, 5, 10, 15, 15} {
		if got := cadencePrototypeDelay(cadencePrototypeInput{demandKnown: true, unchanged: true, terminal: true, healthy: true, stablePasses: n}); got != want*time.Minute {
			t.Fatalf("pass%d got%s", n, got)
		}
	}
}
