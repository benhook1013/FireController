package reviewruntime

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

func newConfigurationCompatibilityFixture(t *testing.T) *hostedDriverFixture {
	t.Helper()
	f := newHostedDriverFixture(t)
	// The hosted fixture installs its final capability fingerprint on a new
	// engine; bind the CLI adapter to that same engine for cross-channel proof.
	fetch := f.driver.fetch
	var err error
	f.driver, err = NewCLIDriver(f.store, f.engine, f.capability, f.manager, f.captures, f.reader, f.runner)
	if err != nil {
		t.Fatal(err)
	}
	f.driver.fetch = fetch
	return f
}

func TestNativeDriversPrepareOriginalConfigurationAcrossSuffixCleanup(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, phase := range []string{"before claim", "during preparation"} {
			t.Run(channel+"/"+phase, func(t *testing.T) {
				f := newConfigurationCompatibilityFixture(t)
				raw, err := f.engine.Enqueue(t.Context(), f.scope(t, f.writer, false), "original-request", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: channel, HeadSHA: f.headSHA})
				if err != nil {
					t.Fatal(err)
				}
				var receipt reviewexec.Receipt
				if err := json.Unmarshal(raw, &receipt); err != nil {
					t.Fatal(err)
				}
				var original reviewexec.Configuration
				var body string
				if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_config WHERE project_id=? AND revision=1`, f.project).Scan(&body); err != nil || json.Unmarshal([]byte(body), &original) != nil {
					t.Fatal("original configuration unavailable", err)
				}
				change := func(revision int64, queue []int64, key string) {
					t.Helper()
					c := original
					c.ExpectedRevision = &revision
					c.Queue = queue
					if _, err := f.engine.Configure(t.Context(), f.scope(t, f.admin, true), key, c); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "before claim" {
					change(1, []int64{2, 4}, "suffix-before-claim")
				}
				a, err := f.engine.StartAttempt(t.Context(), f.project, receipt.ID)
				if err != nil || a.ID == "" {
					t.Fatal("original request could not be claimed", err)
				}
				if phase == "during preparation" {
					change(1, []int64{2, 4}, "suffix-during-preparation")
				}
				current, err := f.engine.ExecutionContext(t.Context(), f.project, receipt.ID)
				if err != nil || current.Operation.ConfigurationRevision != 1 || !reflect.DeepEqual(current.Configuration, original) {
					t.Fatal("execution context discarded original configuration", current.Configuration, err)
				}
				driver := reviewexec.Driver(f.driver)
				if channel == "hosted" {
					driver = f.hosted
				}
				prepared, err := driver.Prepare(t.Context(), current.Operation, original, a)
				if err != nil {
					t.Fatal("native preparation refused suffix-only cleanup", err)
				}
				// Exercise the same guard again at admission and immediately before
				// dispatch, after another suffix-only edit. Never invoke Dispatch.
				change(2, []int64{2, 6}, "suffix-after-preparation")
				a, err = f.engine.Admit(t.Context(), f.project, a.ID, a.Revision, prepared.Anchor, prepared.Intent)
				if err != nil {
					t.Fatal("native prepared request could not be admitted", err)
				}
				if channel == "hosted" {
					source, err := reviewexec.ResolveSource(t.Context(), f.provider, prepared.Anchor.RepositoryID, original, operationRequest(current.Operation))
					if err != nil {
						t.Fatal(err)
					}
					err = f.engine.VerifyHostedAction(t.Context(), f.project, a.ID, a.Revision, source)
					if err != nil || f.provider.queries != 1 || f.provider.reviews != 0 {
						t.Fatal("hosted query or final guard lost original binding", err)
					}
				} else {
					if err := f.engine.VerifyAction(t.Context(), f.project, a.ID, a.Revision); err != nil {
						t.Fatal("CLI final guard refused original binding", err)
					}
					log, err := os.ReadFile(f.log)
					if err != nil || string(log) != "usage --agent\n" {
						t.Fatal("preparation started a review or repeated allowance work", err)
					}
				}
				var retained int64
				if err := f.store.DB().QueryRow(`SELECT configuration_revision FROM review_operations WHERE id=?`, receipt.ID).Scan(&retained); err != nil || retained != 1 {
					t.Fatal("request provenance changed", err)
				}
			})
		}
	}
}

func TestNativeDriverPreparationRejectsMeaningfulRestoredConfiguration(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, scenario := range []string{"prefix restored", "pause restored", "unchanged replacement"} {
			t.Run(channel+"/"+scenario, func(t *testing.T) {
				f := newConfigurationCompatibilityFixture(t)
				var operation reviewexec.Operation
				var attempt reviewexec.Attempt
				var original reviewexec.Configuration
				driver := reviewexec.Driver(f.driver)
				if channel == "hosted" {
					operation, attempt, original = f.startHosted(t)
					driver = f.hosted
				} else {
					operation, attempt, original = f.start(t, 2, 1, "original-cli")
				}
				for index := 0; index < 2; index++ {
					c := original
					revision := int64(index + 1)
					c.ExpectedRevision = &revision
					if index == 0 {
						if scenario == "prefix restored" {
							c.Queue = []int64{4, 2}
						}
						c.Paused = scenario == "pause restored"
					} else {
						c.Queue = []int64{2, 4}
					}
					if _, err := f.engine.Configure(t.Context(), f.scope(t, f.admin, true), []string{"first-change", "restored-change"}[index], c); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := f.engine.ExecutionContext(t.Context(), f.project, operation.ID); err == nil {
					t.Fatal("restored configuration masked meaningful change")
				}
				if _, err := driver.Prepare(t.Context(), operation, original, attempt); err == nil {
					t.Fatal("native driver accepted a meaningfully changed plan")
				}
				if f.provider.queries != 0 || f.provider.reviews != 0 || f.fetches.Load() != 0 {
					t.Fatal("refused preparation reached source materialization or provider action")
				}
			})
		}
	}
}
