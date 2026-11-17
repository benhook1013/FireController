//go:build linux

package reviewruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/capturecustody"
	"github.com/benhook1013/FireController/internal/reviewprocess"
)

func collectorFixture(t *testing.T, kind string) (string, PublishedCaptureBinding, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "captures")
	os.Mkdir(root, 0700)
	var binding PublishedCaptureBinding
	switch kind {
	case "cli":
		store, _ := NewCaptureStore(root)
		ref, err := store.Save(context.Background(), "attempt", captureTestRecord(reviewprocess.Capture{Started: true}))
		if err != nil {
			t.Fatal(err)
		}
		binding = PublishedCaptureBinding{"attempt", ref}
	case "hosted":
		store, _ := NewHostedCaptureStore(root)
		id, record := hostedCaptureFixture(t)
		ref, err := store.Save(context.Background(), id, record)
		if err != nil {
			t.Fatal(err)
		}
		binding = PublishedCaptureBinding{id, ref}
	case "exhaustion":
		store, _ := NewCLIExhaustionStore(root)
		record := exhaustionCustodyFixture()
		if err := store.Save(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		data, err := exhaustionEnvelope(record)
		if err != nil {
			t.Fatal(err)
		}
		binding = PublishedCaptureBinding{record.Attempt.ID, captureReference(data)}
	}
	name := binding.AttemptID + ".json"
	if kind == "hosted" {
		name = hostedCaptureFilename(binding.AttemptID, binding.Reference)
	}
	return root, binding, filepath.Join(root, name)
}

func TestCaptureStagingCollectorOnlyVerifiedNewGenerationDuplicates(t *testing.T) {
	for _, kind := range []string{"cli", "hosted", "exhaustion"} {
		t.Run(kind, func(t *testing.T) {
			root, b, final := collectorFixture(t, kind)
			before, _ := os.ReadFile(final)
			duplicate := filepath.Join(root, ".capture-stage-v1-"+kind+"-duplicate")
			os.Link(final, duplicate)
			legacy := filepath.Join(root, ".capture-legacy")
			os.Link(final, legacy)
			sole := filepath.Join(root, ".capture-stage-v1-"+kind+"-sole")
			os.WriteFile(sole, before, 0600)
			link := filepath.Join(root, ".capture-stage-v1-"+kind+"-symlink")
			os.Symlink(final, link)
			dry, err := CollectPublishedCaptureStaging(context.Background(), root, kind, []PublishedCaptureBinding{b}, false)
			if err != nil || dry.Eligible != 1 || dry.Removed != 0 {
				t.Fatalf("dry=%+v %v", dry, err)
			}
			if _, err = os.Lstat(duplicate); err != nil {
				t.Fatal("dry-run deleted")
			}
			result, err := CollectPublishedCaptureStaging(context.Background(), root, kind, []PublishedCaptureBinding{b}, true)
			if err != nil || result.Removed != 1 {
				t.Fatalf("apply=%+v %v", result, err)
			}
			for _, path := range []string{final, legacy, sole, link} {
				if _, err = os.Lstat(path); err != nil {
					t.Fatal("protected artifact removed", path)
				}
			}
			after, _ := os.ReadFile(final)
			if string(before) != string(after) {
				t.Fatal("final changed")
			}
			again, err := CollectPublishedCaptureStaging(context.Background(), root, kind, []PublishedCaptureBinding{b}, true)
			if err != nil || again.Removed != 0 {
				t.Fatal("retry not idempotent", err)
			}
		})
	}
}

func TestCaptureStagingCollectorRefusesActiveWriterAndUnqualifiedBinding(t *testing.T) {
	root, b, final := collectorFixture(t, "cli")
	stage := filepath.Join(root, ".capture-stage-v1-cli-duplicate")
	os.Link(final, stage)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	writer, err := capturecustody.Writer(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = CollectPublishedCaptureStaging(ctx, root, "cli", []PublishedCaptureBinding{b}, true); !errors.Is(err, capturecustody.ErrBusy) {
		t.Fatal("active writer not protected", err)
	}
	writer.Close()
	bad := b
	bad.Reference.Bytes++
	if _, err = CollectPublishedCaptureStaging(ctx, root, "cli", []PublishedCaptureBinding{bad}, true); err == nil {
		t.Fatal("invalid reference accepted")
	}
	if _, err = os.Stat(stage); err != nil {
		t.Fatal("refusal deleted staging")
	}
	if _, err = CollectPublishedCaptureStaging(ctx, root, "cli", []PublishedCaptureBinding{b, bad}, true); err == nil {
		t.Fatal("contradictory duplicate binding accepted")
	}
}

func TestCaptureStagingCollectorBoundsBeforeMutation(t *testing.T) {
	root, b, final := collectorFixture(t, "cli")
	for i := 0; i < 129; i++ {
		if err := os.Link(final, filepath.Join(root, ".capture-stage-v1-cli-"+time.Unix(int64(i), 0).Format("150405"))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := CollectPublishedCaptureStaging(context.Background(), root, "cli", []PublishedCaptureBinding{b}, true); err == nil {
		t.Fatal("excess plan accepted")
	}
	entries := captureTestEntries(t, root)
	if len(entries) != 130 {
		t.Fatalf("partial deletion on overbudget: %d", len(entries))
	}
}

func TestCaptureStagingCollectorCancelledAndCorruptFinalPreserveNames(t *testing.T) {
	root, b, final := collectorFixture(t, "cli")
	stage := filepath.Join(root, ".capture-stage-v1-cli-duplicate")
	if err := os.Link(final, stage); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CollectPublishedCaptureStaging(ctx, root, "cli", []PublishedCaptureBinding{b}, true); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel ignored", err)
	}
	if err := os.WriteFile(final, []byte("corrupt retained evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectPublishedCaptureStaging(context.Background(), root, "cli", []PublishedCaptureBinding{b}, true); err == nil {
		t.Fatal("corrupt final accepted")
	}
	if _, err := os.Stat(stage); err != nil {
		t.Fatal("refusal deleted duplicate")
	}
}
