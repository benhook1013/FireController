package reviewruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheAccountCustodyDefersUntilProducerReturns(t *testing.T) {
	g := &cacheAccountGate{}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	go func() { done <- g.withExclusive(ctx, func() error { close(entered); <-release; return ctx.Err() }) }()
	<-entered
	cancel()
	called := false
	if err := g.withExclusive(t.Context(), func() error { called = true; return nil }); err == nil || called {
		t.Fatal("caller cancellation released producer custody")
	}
	close(release)
	if !errors.Is(<-done, context.Canceled) {
		t.Fatal("producer completion lost cancellation")
	}
	if err := g.withExclusive(t.Context(), func() error { called = true; return nil }); err != nil || !called {
		t.Fatal(err)
	}
}
func TestCacheAccountCustodyRefusesCancelledAndReleasesOnError(t *testing.T) {
	g := &cacheAccountGate{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	if err := g.withExclusive(ctx, func() error { called = true; return nil }); err == nil || called {
		t.Fatal("cancel admitted")
	}
	if err := g.withExclusive(t.Context(), func() error { return errors.New("failure") }); err == nil {
		t.Fatal("error lost")
	}
	if err := g.withExclusive(t.Context(), func() error { return nil }); err != nil {
		t.Fatal("lock leaked", err)
	}
}

func TestCacheCaptureObservationUsesNativeSettlementAndExactRetainedFile(t *testing.T) {
	f := newCLIEvidenceFixture(t)
	a, w := f.startAdmitted(t, 2)
	capture := makeCompleteCLICapture(t, w, mustChangedFiles(t, f.manager, w))
	start, finish := time.Now().Add(-time.Second).UTC(), time.Now().UTC()
	if _, err := RetainCLI(t.Context(), f.captures, f.engine, f.manager, a, w, capture, start, finish); err != nil {
		t.Fatal(err)
	}
	gate := &cacheAccountGate{}
	now := time.Now().Add(time.Hour)
	if got, err := observeCacheCapture(t.Context(), gate, f.engine, f.captures, "runtime-capability", a.ID, now); err != nil || got.AttemptID != "" {
		t.Fatal("active attempt observed settled", got, err)
	}
	if _, err := f.engine.Advance(t.Context(), f.project, a.ID, a.Revision, "completed", "completion", ""); err != nil {
		t.Fatal(err)
	}
	got, err := observeCacheCapture(t.Context(), gate, f.engine, f.captures, "runtime-capability", a.ID, now)
	if err != nil || got.AttemptID != a.ID {
		t.Fatal(got, err)
	}
	path := filepath.Join(f.captures.root, a.ID+".json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(original, byte(' ')), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := observeCacheCapture(t.Context(), gate, f.engine, f.captures, "runtime-capability", a.ID, now); err == nil || got.AttemptID != "" {
		t.Fatal("changed capture accepted", got, err)
	}
	if _, err := os.Stat(w.Directory); err != nil {
		t.Fatal("observation changed workspace", err)
	}
}
