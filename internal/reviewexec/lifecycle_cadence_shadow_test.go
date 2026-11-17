package reviewexec

import (
	"bytes"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCadenceShadowClassificationAndNoMutation(t *testing.T) {
	now := time.Unix(100, 0)
	stable := lifecycleDue{head: "h", ciHead: "h", baseSHA: "b", ciBaseSHA: "b", baseRef: "main", ciBaseRef: "main", ciGateState: "success"}
	s := &lifecycleSchedule{due: map[lifecycleKey]lifecycleDue{}, watches: map[ciWatchKey]*ciWatchEntry{}}
	for i := int64(1); i <= 4; i++ {
		s.due[lifecycleKey{"private-project", i}] = stable
	}
	s.watches[ciWatchKey{binding: ciWatchBinding{project: "private-project"}, number: 2}] = &ciWatchEntry{owners: map[*ciWatchSubscription]bool{&ciWatchSubscription{}: true}}
	d := stable
	d.hints[0].wanted = 2
	d.hints[0].done = 1
	s.due[lifecycleKey{"private-project", 3}] = d
	d = stable
	d.head = "moved"
	d.ciError = "private-error"
	s.due[lifecycleKey{"private-project", 4}] = d
	before := make(map[lifecycleKey]lifecycleDue)
	for k, v := range s.due {
		before[k] = v
	}
	x := s.cadenceShadowLocked(now)
	if x.Total != 4 || x.Sampled != 4 || x.Watched != 1 || x.DirtyHint != 1 || x.Changed != 1 || x.Unhealthy != 1 || x.LocallyQuiet != 1 || x.DurableDemandUnknown != 4 {
		t.Fatalf("%+v", x)
	}
	if !reflect.DeepEqual(before, s.due) {
		t.Fatal("diagnostics changed scheduling")
	}
	if s.cadenceShadowLocked(now.Add(59*time.Second)) != nil {
		t.Fatal("not throttled")
	}
	if s.cadenceShadowLocked(now.Add(time.Minute)) == nil {
		t.Fatal("no next snapshot")
	}
}
func TestCadenceShadowBoundedAndUnknown(t *testing.T) {
	s := &lifecycleSchedule{due: map[lifecycleKey]lifecycleDue{}}
	for i := 0; i < 1000; i++ {
		s.due[lifecycleKey{fmt.Sprint(i), 1}] = lifecycleDue{}
	}
	x := s.cadenceShadowLocked(time.Unix(100, 0))
	if x.Total != 1000 || x.Sampled != 256 || !x.Truncated || x.DurableDemandUnknown != 256 || x.LocallyQuiet != 0 {
		t.Fatalf("%+v", x)
	}
}

func TestCadenceShadowLogIsAggregateOnly(t *testing.T) {
	var out bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	defer slog.SetDefault(old)
	s := &lifecycleSchedule{due: map[lifecycleKey]lifecycleDue{{"PRIVATE-PROJECT", 12345}: {head: "PRIVATE-SHA", ciError: "PRIVATE-ERROR"}}}
	logCadenceShadow(s.cadenceShadowLocked(time.Unix(100, 0)))
	logCadenceShadow(nil)
	if strings.Contains(out.String(), "PRIVATE") || strings.Contains(out.String(), "12345") || strings.Count(out.String(), "Lifecycle cadence shadow") != 1 {
		t.Fatal(out.String())
	}
	if !strings.Contains(out.String(), `"durable_demand_unknown":1`) {
		t.Fatal(out.String())
	}
}

func TestCadenceShadowWatchOverflowIsExplicit(t *testing.T) {
	s := &lifecycleSchedule{due: map[lifecycleKey]lifecycleDue{{"p", 1}: {head: "h", ciHead: "h", baseSHA: "b", ciBaseSHA: "b", baseRef: "main", ciBaseRef: "main", ciGateState: "success"}}, watches: map[ciWatchKey]*ciWatchEntry{}}
	for i := int64(1); i <= 257; i++ {
		s.watches[ciWatchKey{binding: ciWatchBinding{project: "p"}, number: i}] = &ciWatchEntry{owners: map[*ciWatchSubscription]bool{&ciWatchSubscription{}: true}}
	}
	x := s.cadenceShadowLocked(time.Unix(100, 0))
	if !x.WatchIndexTruncated || x.Truncated || x.Sampled != 1 || x.LocallyQuiet != 0 {
		t.Fatalf("%+v", x)
	}
}

func TestCadenceShadowDisabledHasNoSideEffects(t *testing.T) {
	s := &lifecycleSchedule{options: LifecycleSyncOptions{Now: func() time.Time { t.Fatal("disabled diagnostic read clock"); return time.Time{} }}}
	var engine *Engine // Disabled diagnostics must not reach the local database.
	s.observeCadenceShadow()
	engine.observeCadenceDemand(t.Context(), s)
	if !s.shadowNext.IsZero() || !s.demandShadowNext.IsZero() {
		t.Fatal("disabled diagnostic changed throttle")
	}
	options, err := lifecycleOptions(nil)
	if err != nil || options.CadenceShadow {
		t.Fatal("diagnostic enabled by default")
	}
	options, err = lifecycleOptions([]LifecycleSyncOptions{{CadenceShadow: true}})
	if err != nil || !options.CadenceShadow {
		t.Fatal("explicit opt-in lost")
	}
}
