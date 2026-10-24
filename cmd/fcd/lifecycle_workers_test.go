package main

import (
	"context"
	"github.com/benhook1013/FireController/internal/reviewruntime"
	"io"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestLifecycleWorkersOwnedByDaemonAndEmptyEnrollmentStaysLocal(t *testing.T) {
	store, _ := sourceWorkersFixture(t)
	if err := reviews.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	engine, err := reviewexec.New(store, []reviewexec.Capability{{ID: "source-capability", Provider: "coderabbit", Account: "source-account", Channels: []string{"cli"}}}, reviewexec.WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	r := &sourceWorkerReader{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := startReviewWorkers(ctx, engine, map[string]reviewexec.Driver{"source-capability": &sourceBoundDriver{reader: r}}, io.Discard)
	if err != nil || done == nil {
		t.Fatal(done, err)
	}
	if duplicate, err := engine.StartLifecycleSync(t.Context(), map[string]reviewexec.TargetReader{"source-capability": r}); err == nil || duplicate != nil {
		t.Fatal("daemon does not own lifecycle observer")
	}
	cancel()
	waitSourceWorkers(t, done)
	if r.calls.Load() != 0 {
		t.Fatal("empty native enrollment caused provider work")
	}
	ctx2, cancel2 := context.WithCancel(t.Context())
	again, err := engine.StartLifecycleSync(ctx2, map[string]reviewexec.TargetReader{"source-capability": r})
	if err != nil || again == nil {
		t.Fatal("owner not released after join", again, err)
	}
	cancel2()
	waitSourceWorkers(t, again)
}

func TestLifecycleCombinedDriverRetainsHostedObservationIdentity(t *testing.T) {
	for _, channels := range [][]string{{"cli"}, {"hosted"}, {"cli", "hosted"}} {
		t.Run(strings.Join(channels, "+"), func(t *testing.T) {
			store, _ := sourceWorkersFixture(t)
			_, catalog := privateTestCatalog(t, channels)
			_, drivers, err := buildReviewExecution(store, t.TempDir(), catalog)
			if err != nil {
				t.Fatal(err)
			}
			driver := drivers["local-cli"]
			fallback := driver.(sourceReaderProvider).SourceReader()
			reader := lifecycleReader(driver, fallback)
			hosted, ok := reader.(reviewexec.ExternalHostedReader)
			if len(channels) == 1 && channels[0] == "cli" {
				if ok || reader != fallback {
					t.Fatal("CLI invented hosted observer")
				}
				return
			}
			if !ok || hosted.TrustedHostedActor().ID != 20 || hosted.TrustedHostedActor().Kind != "Bot" || hosted.TrustedHostedActor().Login != "synthetic-provider" {
				t.Fatal("real daemon composition lost trusted hosted actor", reader)
			}
			if len(channels) == 2 {
				combined, ok := driver.(*reviewChannelDriver)
				if !ok || fallback != combined.cli.SourceReader() || reader == fallback {
					t.Fatal("dual driver retained CLI-only lifecycle view")
				}
			}
		})
	}
	var nilHosted *reviewruntime.HostedDriver
	if nilHosted.HostedObservationReader() != nil || (&reviewruntime.HostedDriver{}).HostedObservationReader() != nil {
		t.Fatal("nil/unconfigured driver exposed observer")
	}
}
