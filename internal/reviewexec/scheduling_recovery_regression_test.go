package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func cliSourceRetryFixture(t *testing.T) (*attemptFixture, Run) {
	t.Helper()
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingCLIEnable(t, f, 0, true)
	schedulingCLIGenerate(t, f, true)
	selectionNext(t, f, &refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
		return reviewprovider.Repository{}, context.DeadlineExceeded
	}})
	r := schedulingLatest(t, f)
	if r.Channel != "cli" || r.State != "held" || r.Revision != 3 || r.OperationID != "" {
		t.Fatal(r)
	}
	return f, r
}

func TestAutomaticCLISourceRetryKeepsOriginalIntentAndBudget(t *testing.T) {
	for _, mode := range []string{"success", "budget"} {
		t.Run(mode, func(t *testing.T) {
			f, original := cliSourceRetryFixture(t)
			schedulingCLIGenerate(t, f, false)
			for _, revision := range []int64{6, 9} {
				ageSourceRetry(t, f, original.ID)
				restarted, err := New(f.store, attemptCapabilities)
				if err != nil {
					t.Fatal(err)
				}
				f.e = restarted
				if err = f.e.RecoverReviewRuns(t.Context()); err != nil {
					t.Fatal(err)
				}
				schedulingCLIGenerate(t, f, true)
				r := schedulingLatest(t, f)
				if r.ID != original.ID || r.ActorID != original.ActorID || r.WorkerID != original.WorkerID || r.ConfigurationRevision != original.ConfigurationRevision || r.PolicyRevision != original.PolicyRevision {
					t.Fatal("recovery replaced intent", r)
				}
				schedulingCLIGenerate(t, f, false)
				reader := &refreshTestReader{}
				if mode == "budget" {
					reader.repository = func(context.Context, int64) (reviewprovider.Repository, error) {
						return reviewprovider.Repository{}, context.DeadlineExceeded
					}
				}
				selectionNext(t, f, reader)
				r = schedulingLatest(t, f)
				if r.Revision != revision {
					t.Fatal(r)
				}
				if mode == "success" {
					if r.State != "selected" || r.OperationID == "" {
						t.Fatal(r)
					}
					if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 {
						t.Fatal("recovery duplicated child")
					}
					break
				}
				if r.State != "held" || r.OperationID != "" {
					t.Fatal(r)
				}
			}
			ageSourceRetry(t, f, original.ID)
			schedulingCLIGenerate(t, f, false)
			if attemptCount(t, f.store, `SELECT count(*) FROM review_selections`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
				t.Fatal("recovery manufactured intent or provider work")
			}
		})
	}
}

func TestAutomaticCLISourceRetrySafetyFences(t *testing.T) {
	for _, mode := range []string{"revoked", "disabled", "pause", "restore", "later-cli", "later-hosted", "rate-limited", "configuration"} {
		t.Run(mode, func(t *testing.T) {
			f, r := cliSourceRetryFixture(t)
			ageSourceRetry(t, f, r.ID)
			switch mode {
			case "revoked":
				admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
			case "disabled":
				schedulingCLIEnable(t, f, 1, false)
			case "pause":
				schedulingPause(t, f, 2, 0, true)
			case "restore":
				admissionSQL(t, f, `INSERT INTO metadata VALUES('restore:provider_state','held')`)
			case "later-cli", "later-hosted":
				channel := "cli"
				if mode == "later-hosted" {
					channel = "hosted"
				}
				later, _ := selectionEnqueue(t, f, "later-recovery-fence", channel, nil)
				// A later settled intent must still prevent revival of the old one.
				admissionSQL(t, f, `UPDATE review_selections SET state='held',reason='selection_changed',revision=revision+1 WHERE id=?`, later.ID)
			case "rate-limited":
				admissionSQL(t, f, `UPDATE review_selections SET reason='source_rate_limited' WHERE id=?`, r.ID)
			case "configuration":
				f.configure(t, attemptProject, "other-account", 1, false)
			}
			if mode == "pause" || mode == "restore" {
				schedulingCLIGenerate(t, f, false)
				if got := selectionRead(t, f, r.ID); got.State != "held" || got.Revision != r.Revision {
					t.Fatal("coordinator safety hold revived selection", got)
				}
				return
			}
			// Deliberate control changes can authorize a new semantic intent; inspect only
			// whether this exact retained source hold may be recovered under its authority.
			var worked bool
			err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				var basis string
				if err := tx.QueryRowContext(ctx, `SELECT settled_basis_sha256 FROM review_automatic_selections WHERE selection_id=?`, r.ID).Scan(&basis); err != nil {
					return err
				}
				s, err := readScheduling(ctx, tx, r.ProjectID)
				if err != nil {
					return err
				}
				worked, err = f.e.retryAutomaticSourceChannelTx(ctx, tx, r.ProjectID, basis, s.Revision, time.Now(), "cli")
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if worked != (mode == "later-hosted") {
				t.Fatalf("%s: recovery=%v", mode, worked)
			}
		})
	}
}

func TestAutomaticGeneratorsIsolateRetainedParentCycle(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		t.Run(channel, func(t *testing.T) {
			f := schedulingFixture(t)
			f.configure(t, otherAttemptProject, "test-capability", 0, false)
			admissionSQL(t, f, `UPDATE review_execution_config SET body_json=json_set(body_json,'$.mode','stacked') WHERE project_id=?`, attemptProject)
			now, sha := stamp(), strings.Repeat("a", 40)
			prefix := []frontPull{{Number: 2, State: "open", HeadSHA: sha, BaseSHA: sha, BaseRef: "four", ParentPullNumber: 4, ParentSHA: sha, ObservedAt: now}, {Number: 4, State: "open", HeadSHA: sha, BaseSHA: sha, BaseRef: "two", ParentPullNumber: 2, ParentSHA: sha, ObservedAt: now}}
			raw, err := json.Marshal(prefix)
			if err != nil {
				t.Fatal(err)
			}
			admissionSQL(t, f, `INSERT INTO review_front_frames VALUES(?,?,801,1,?,?,1,'',?)`, attemptProject, channel, now, now, string(raw))
			// Prove the fixture reaches the narrow cycle error, not generic corruption.
			err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				_, err := automaticDependenciesChannel(ctx, tx, attemptProject, 2, channel)
				return err
			})
			if !errors.Is(err, errAutomaticParentCycle) {
				t.Fatal("invalid cycle fixture", err)
			}
			for _, project := range []string{attemptProject, otherAttemptProject} {
				zero := int64(0)
				hosted, cli := channel == "hosted", channel == "cli"
				_, err := f.e.ConfigureScheduling(t.Context(), selectionScope(t, f, project, f.admin, "write"), core.NewID("enable"), SchedulingRequest{ExpectedRevision: &zero, AutomaticHosted: &hosted, AutomaticCLI: &cli})
				if err != nil {
					t.Fatal(err)
				}
			}
			readers := map[string]TargetReader{"test-capability": &refreshTestReader{}}
			generate := f.e.generateAutomaticHosted
			if channel == "cli" {
				generate = f.e.generateAutomaticCLI
			}
			worked, err := generate(t.Context(), readers)
			if err != nil || !worked {
				t.Fatal("project cycle stopped healthy project", worked, err)
			}
			var project string
			if err := f.store.DB().QueryRow(`SELECT project_id FROM review_automatic_selections`).Scan(&project); err != nil || project != otherAttemptProject {
				t.Fatal(project, err)
			}
			// Shared storage failure must still reach the coordinator rather than being hidden.
			admissionSQL(t, f, `ALTER TABLE review_front_frames RENAME TO missing_front_frames`)
			if _, err := generate(t.Context(), readers); err == nil {
				t.Fatal("shared storage failure was swallowed")
			}
		})
	}
}

func TestAutomaticCLISourceRetryRechecksFencesAfterSourceIO(t *testing.T) {
	for _, mode := range []string{"cancelled-operation", "scheduling", "configuration", "policy"} {
		t.Run(mode, func(t *testing.T) {
			f, r := cliSourceRetryFixture(t)
			ageSourceRetry(t, f, r.ID)
			schedulingCLIGenerate(t, f, true)
			var once sync.Once
			reader := &refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
				once.Do(func() {
					switch mode {
					case "cancelled-operation":
						raw, err := f.e.Enqueue(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "during-source", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, HeadSHA: strings.Repeat("a", 40), Channel: "cli"})
						if err != nil {
							t.Fatal(err)
						}
						var receipt Receipt
						if err = json.Unmarshal(raw, &receipt); err != nil {
							t.Fatal(err)
						}
						_, err = f.e.ControlOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "cancel-during-source", receipt.ID, OperationControl{ExpectedRevision: 1, Action: "cancel"})
						if err != nil {
							t.Fatal(err)
						}
					case "scheduling":
						schedulingCLIEnable(t, f, 1, true)
					case "configuration":
						f.configure(t, attemptProject, "test-capability", 1, false)
					case "policy":
						assessmentPolicy(t, f, 1, []string{"cli"}, []string{})
					}
				})
				return reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, nil
			}}
			selectionNext(t, f, reader)
			got := selectionRead(t, f, r.ID)
			if got.State != "held" || got.OperationID != "" {
				t.Fatal("changed authority or cancelled successor ignored", got)
			}
			expected := 0
			if mode == "cancelled-operation" {
				expected = 1
				if got.Reason != "selection_superseded" {
					t.Fatal(got)
				}
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != expected {
				t.Fatal("retry created a child after its fence changed")
			}
		})
	}
}
