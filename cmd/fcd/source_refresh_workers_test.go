package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviewruntime"
)

type sourceWorkerReader struct{ calls atomic.Int32 }

func (r *sourceWorkerReader) Repository(context.Context, int64) (reviewprovider.Repository, error) {
	r.calls.Add(1)
	return reviewprovider.Repository{}, errors.New("unexpected idle provider read")
}
func (r *sourceWorkerReader) Pull(context.Context, string, int64) (reviewprovider.Pull, error) {
	r.calls.Add(1)
	return reviewprovider.Pull{}, errors.New("unexpected idle provider read")
}

type sourceWorkerDriver struct{}

func (*sourceWorkerDriver) Prepare(context.Context, reviewexec.Operation, reviewexec.Configuration, reviewexec.Attempt) (reviewexec.Preparation, error) {
	return reviewexec.Preparation{}, errors.New("unexpected idle preparation")
}
func (*sourceWorkerDriver) Dispatch(context.Context, reviewexec.Operation, reviewexec.Attempt, func(context.Context, string) error) (reviewexec.Result, error) {
	return reviewexec.Result{}, errors.New("unexpected idle dispatch")
}

type sourceBoundDriver struct {
	sourceWorkerDriver
	reader     reviewexec.TargetReader
	selections atomic.Int32
}

func (d *sourceBoundDriver) SourceReader() reviewexec.TargetReader {
	d.selections.Add(1)
	return d.reader
}

type sourceStartupObserver struct {
	sourceBoundDriver
	canceled chan struct{}
	release  chan struct{}
}

func (d *sourceStartupObserver) StartRecovery(ctx context.Context) (<-chan error, error) {
	done := make(chan error, 1)
	go func() { <-ctx.Done(); close(d.canceled); <-d.release; done <- nil; close(done) }()
	return done, nil
}

func sourceWorkersFixture(t *testing.T) (*core.Store, *reviewexec.Engine) {
	t.Helper()
	store, err := core.Open(t.Context(), filepath.Join(t.TempDir(), "private"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := reviewexec.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	engine, err := reviewexec.New(store, []reviewexec.Capability{{ID: "source-capability", Provider: "coderabbit", Account: "source-account", Channels: []string{"cli"}}})
	if err != nil {
		t.Fatal(err)
	}
	return store, engine
}
func waitSourceWorkers(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon worker aggregate did not settle")
	}
}

func TestSourceReaderCompositionModesAndNilSafety(t *testing.T) {
	var cli *reviewruntime.CLIDriver
	var hosted *reviewruntime.HostedDriver
	if cli.SourceReader() != nil || hosted.SourceReader() != nil || (&reviewruntime.CLIDriver{}).SourceReader() != nil || (&reviewruntime.HostedDriver{}).SourceReader() != nil || (*reviewChannelDriver)(nil).SourceReader() != nil {
		t.Fatal("nil driver exposed a source reader")
	}
	for _, mode := range []string{"disabled", "cli", "hosted", "dual"} {
		t.Run(mode, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "private")
			store, err := core.Open(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := reviewexec.EnsureSchema(t.Context(), store); err != nil {
				t.Fatal(err)
			}
			catalog, err := reviewruntime.LoadCatalog("")
			if err != nil {
				t.Fatal(err)
			}
			if mode != "disabled" {
				channels := []string{mode}
				if mode == "dual" {
					channels = []string{"cli", "hosted"}
				}
				_, catalog = privateTestCatalog(t, channels)
			}
			_, drivers, err := buildReviewExecution(store, dir, catalog)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "disabled" {
				if len(drivers) != 0 {
					t.Fatal("disabled setup exposed a source reader")
				}
				return
			}
			reader := drivers["local-cli"].(sourceReaderProvider).SourceReader()
			if reader == nil {
				t.Fatal("configured channel has no source reader")
			}
			if _, ok := reader.(*reviewprovider.GitHub); !ok {
				t.Fatal("source reader differs from configured provider")
			}
			switch driver := drivers["local-cli"].(type) {
			case *reviewruntime.CLIDriver:
				cli = driver
			case *reviewruntime.HostedDriver:
				hosted = driver
			case *reviewChannelDriver:
				if reader != driver.cli.SourceReader() || reader != driver.hosted.SourceReader() {
					t.Fatal("dual capability did not expose exactly its shared provider")
				}
			}
		})
	}
	if cli == nil || hosted == nil || cli.SourceReader() == hosted.SourceReader() {
		t.Fatal("independent operator fixtures did not create distinct bindings")
	}
	if (&reviewChannelDriver{cli: cli, hosted: hosted}).SourceReader() != cli.SourceReader() || (&reviewChannelDriver{hosted: hosted}).SourceReader() != hosted.SourceReader() {
		t.Fatal("channel router did not select configured reader deterministically")
	}
	if (&reviewChannelDriver{cli: &reviewruntime.CLIDriver{}, hosted: hosted}).SourceReader() != nil {
		t.Fatal("source accessor fell back from a configured CLI adapter")
	}
}

func TestSourceRefreshWorkersAreOwnedAndIdleReadsStayLocal(t *testing.T) {
	_, engine := sourceWorkersFixture(t)
	reader := &sourceWorkerReader{}
	driver := &sourceBoundDriver{reader: reader}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := startReviewWorkers(ctx, engine, map[string]reviewexec.Driver{"source-capability": driver})
	if err != nil || done == nil {
		t.Fatal("worker startup", err)
	}
	// A second owner cannot acquire the source worker while the aggregate owns it.
	duplicate, err := engine.StartSourceRefresh(t.Context(), map[string]reviewexec.TargetReader{"source-capability": reader})
	if err == nil || duplicate != nil {
		cancel()
		waitSourceWorkers(t, done)
		t.Fatal("daemon did not own its source worker")
	}
	cancel()
	waitSourceWorkers(t, done)
	if driver.selections.Load() != 1 || reader.calls.Load() != 0 {
		t.Fatal("startup/idle selected multiple readers or contacted provider", driver.selections.Load(), reader.calls.Load())
	}
	// Aggregate completion includes source ownership release, so restart succeeds.
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	next, err := engine.StartSourceRefresh(ctx2, map[string]reviewexec.TargetReader{"source-capability": reader})
	if err != nil || next == nil {
		t.Fatal("source worker outlived aggregate", err)
	}
	cancel2()
	waitSourceWorkers(t, next)
}

func TestSourceRefreshSourceLessFakeDriverKeepsExistingLifecycle(t *testing.T) {
	_, engine := sourceWorkersFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := startReviewWorkers(ctx, engine, map[string]reviewexec.Driver{"source-capability": &sourceWorkerDriver{}})
	if err != nil || done == nil {
		t.Fatal(err)
	}
	reader := &sourceWorkerReader{}
	sourceCtx, sourceCancel := context.WithCancel(t.Context())
	defer sourceCancel()
	separate, err := engine.StartSourceRefresh(sourceCtx, map[string]reviewexec.TargetReader{"source-capability": reader})
	if err != nil || separate == nil {
		t.Fatal("driver without accessor started a source loop", err)
	}
	sourceCancel()
	waitSourceWorkers(t, separate)
	cancel()
	waitSourceWorkers(t, done)
}

func TestSourceRefreshStartupFailureCancelsAndAwaitsObserver(t *testing.T) {
	_, engine := sourceWorkersFixture(t)
	// Advertising an unavailable reader fails source startup after the existing
	// observer starts; the shared owner must cancel AND await that observer.
	driver := &sourceStartupObserver{canceled: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := startReviewWorkers(ctx, engine, map[string]reviewexec.Driver{"source-capability": driver})
	if err == nil || done == nil {
		close(driver.release)
		t.Fatal("invalid source binding did not return owned startup failure", err)
	}
	select {
	case <-driver.canceled:
	case <-time.After(3 * time.Second):
		close(driver.release)
		t.Fatal("source startup failure did not cancel observer")
	}
	select {
	case <-done:
		close(driver.release)
		t.Fatal("aggregate finished before observer settled")
	default:
	}
	close(driver.release)
	waitSourceWorkers(t, done)
	if driver.selections.Load() != 1 {
		t.Fatal("startup selected reader more than once")
	}
}

func TestSourceRefreshRuntimeFailureCancelsAndAwaitsAllWorkers(t *testing.T) {
	store, engine := sourceWorkersFixture(t)
	reader := &sourceWorkerReader{}
	driver := &sourceStartupObserver{sourceBoundDriver: sourceBoundDriver{reader: reader}, canceled: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := startReviewWorkers(ctx, engine, map[string]reviewexec.Driver{"source-capability": driver})
	if err != nil || done == nil {
		close(driver.release)
		t.Fatal("worker startup", err)
	}
	// Fail only the source worker's local queue read. The existing supervisor
	// and observer must be canceled and awaited by the same aggregate owner.
	err = store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DROP TABLE review_source_refreshes`)
		return err
	})
	if err != nil {
		close(driver.release)
		t.Fatal(err)
	}
	select {
	case <-driver.canceled:
	case <-time.After(3 * time.Second):
		close(driver.release)
		t.Fatal("source failure did not cancel sibling observer")
	}
	select {
	case <-done:
		close(driver.release)
		t.Fatal("runtime failure returned before observer settled")
	default:
	}
	close(driver.release)
	select {
	case err := <-done:
		var failure *core.Error
		if !errors.As(err, &failure) || failure.Code != "source_refresh_persistence_failed" {
			t.Fatal("source failure was lost by lifecycle aggregate", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runtime failure did not settle all worker ownership")
	}
	if reader.calls.Load() != 0 {
		t.Fatal("idle source worker contacted provider")
	}
}
