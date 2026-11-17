//go:build linux

package reviewruntime

import (
	"context"
	"github.com/benhook1013/FireController/internal/capturecustody"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func exhaustionCustodyFixture() reviewexec.CLIExhaustionRecovery {
	id := "review_attempt_" + strings.Repeat("a", 32)
	op := "review_operation_" + strings.Repeat("b", 32)
	stamp := "2026-01-02T03:04:05Z"
	sha := strings.Repeat("c", 40)
	hash := strings.Repeat("d", 64)
	return reviewexec.CLIExhaustionRecovery{
		Operation:   reviewexec.Operation{ID: op, AttemptID: id, ProjectID: "project", ActorID: "actor", State: "preparing", Channel: "cli", ConfigurationRevision: 1, PolicyRevision: 1, Revision: 2, CapabilityFingerprint: hash},
		Attempt:     reviewexec.Attempt{ID: id, OperationID: op, ProjectID: "project", State: "preparing", Revision: 1, Provider: "provider", Account: "account"},
		Observation: reviewexec.CLIAllowanceObservation{ObservedAt: stamp, Limit: 10, RollingWindowMS: 1000, ChangedFiles: 1, Anchor: reviewexec.Anchor{ObservedAt: stamp, RepositoryID: 1, Repository: "owner/repo", PullNumber: 1, BaseRef: "develop", CandidateSHA: sha, BaseSHA: sha, ParentSHA: sha, MergeBaseSHA: sha, PatchSHA256: hash}},
	}
}

func TestCapturePublicationAllStoresHonorCustody(t *testing.T) {
	for _, kind := range []string{"cli", "hosted", "exhaustion"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "captures")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			var save func(context.Context) error
			switch kind {
			case "cli":
				store, err := NewCaptureStore(root)
				if err != nil {
					t.Fatal(err)
				}
				save = func(ctx context.Context) error {
					_, err := store.Save(ctx, "attempt", captureTestRecord(reviewprocess.Capture{Started: true}))
					return err
				}
			case "hosted":
				store, err := NewHostedCaptureStore(root)
				if err != nil {
					t.Fatal(err)
				}
				id, record := hostedCaptureFixture(t)
				save = func(ctx context.Context) error { _, err := store.Save(ctx, id, record); return err }
			case "exhaustion":
				store, err := NewCLIExhaustionStore(root)
				if err != nil {
					t.Fatal(err)
				}
				record := exhaustionCustodyFixture()
				if err = reviewexec.ValidateCLIExhaustionRecovery(record); err != nil {
					t.Fatal(err)
				}
				save = func(ctx context.Context) error { return store.Save(ctx, record) }
			}
			collector, err := capturecustody.Collector(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			defer collector.Close()
			short, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			if err = save(short); err == nil {
				t.Fatal("writer bypassed exclusive custody")
			}
			cancel()
			if entries := captureTestEntries(t, root); len(entries) != 0 {
				t.Fatal("blocked writer created staging or final artifact")
			}
			collector.Close()
			if err = save(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err = save(context.Background()); err != nil {
				t.Fatal("replay failed", err)
			}
			if entries := captureTestEntries(t, root); len(entries) != 1 {
				t.Fatalf("unexpected artifacts: %d", len(entries))
			}
			collector, err = capturecustody.Collector(context.Background(), root)
			if err != nil {
				t.Fatal("writer custody leaked", err)
			}
			collector.Close()
		})
	}
}
