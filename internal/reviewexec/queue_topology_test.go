package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestAutomaticSelectionUsesBrowserTopologyForBothChannels(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		t.Run(channel, func(t *testing.T) {
			f := queueEnrollmentFixture(t)
			configureQueue(t, f, 0, 2, 4, 6)
			admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'selection-worker','Selector')`, attemptProject)
			admissionSQL(t, f, `UPDATE grants SET can_manage=1 WHERE credential_id='exec-writer-cred'`)
			pulls := publishQueueTopology(t, f)
			if got := frontRead(t, f); !slices.Equal(got.Queue, []int64{6, 2, 4}) || frontChannel(t, got, channel).QueuedPriority.PullNumber != 6 {
				t.Fatal(got)
			}
			enabled := true
			config := SchedulingRequest{ExpectedRevision: adjudicationRevision(0), AutomaticHosted: new(false)}
			if channel == "cli" {
				config.AutomaticCLI = &enabled
			} else {
				config.AutomaticHosted = &enabled
			}
			if _, err := f.e.ConfigureScheduling(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), "order-enable", config); err != nil {
				t.Fatal(err)
			}
			reader := &refreshTestReader{pull: func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) { return pulls[n], nil }}
			readers := map[string]TargetReader{"test-capability": reader}
			if ok, err := f.e.generateAutomaticProject(t.Context(), attemptProject, channel, readers); err != nil || !ok {
				t.Fatal(ok, err)
			}
			selectionNext(t, f, reader)
			r := schedulingLatest(t, f)
			if r.State != "selected" {
				t.Fatal(r)
			}
			op := bindingOperation(t, f, selectionScope(t, f, attemptProject, f.writer, "read"), r.OperationID)
			if op.PullNumber != 6 {
				t.Fatal(op)
			}
			if got := frontRead(t, f); !slices.Equal(got.Queue, []int64{6, 2, 4}) {
				t.Fatal(got)
			}
			var stored []int64
			var raw string
			if err := f.store.DB().QueryRow(`SELECT json_extract(body_json,'$.queue') FROM review_execution_config WHERE project_id=? ORDER BY revision DESC LIMIT 1`, attemptProject).Scan(&raw); err != nil || json.Unmarshal([]byte(raw), &stored) != nil || !slices.Equal(stored, []int64{2, 4, 6}) {
				t.Fatal(raw, err)
			}
		})
	}
}

func TestQueuedReviewPriorityUsesConfiguredSortMode(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		for _, mode := range []string{"auto", "manual"} {
			t.Run(channel+"/"+mode, func(t *testing.T) {
				f := queueEnrollmentFixture(t)
				f.e.queueSortingWrites = true
				c := enrollmentConfiguration(0, []int64{2, 4, 6}, []string{"cli", "hosted"})
				c.QueueSortMode = mode
				if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "priority-config", c); err != nil {
					t.Fatal(err)
				}
				publishQueueTopology(t, f)
				child := f.enqueue(t, attemptProject, "priority-child", channel, 4, 1)
				root := f.enqueue(t, attemptProject, "priority-root", channel, 2, 1)
				standalone := f.enqueue(t, attemptProject, "priority-standalone", channel, 6, 1)
				first, later := standalone, root
				wantBlockers := []string{standalone.ID, root.ID}
				if mode == "manual" {
					first, later = root, standalone
					wantBlockers = []string{root.ID}
				}
				if a, err := f.e.StartAttempt(t.Context(), attemptProject, later.ID); err != nil || a.ID != "" {
					t.Fatalf("lower-priority operation claimed an attempt: %+v, %v", a, err)
				}
				if err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					o, err := readOperation(ctx, tx, attemptProject, child.ID)
					if err != nil {
						return err
					}
					config, revision, err := readConfiguration(ctx, tx, attemptProject)
					if err != nil {
						return err
					}
					prior, err := f.e.earlierQueuedReviewIntent(ctx, tx, o, config, revision)
					if err == nil && (prior == nil || prior.ID != first.ID) {
						t.Errorf("child's first blocker = %+v; want %s", prior, first.ID)
					}
					if err != nil {
						return err
					}
					for _, limit := range []int{0, 1, 3} {
						blockers, err := f.e.earlierQueuedReviewIntents(ctx, tx, o, config, revision, limit)
						if err != nil {
							return err
						}
						var ids []string
						for _, blocker := range blockers {
							ids = append(ids, blocker.ID)
						}
						if want := wantBlockers[:min(limit, len(wantBlockers))]; !slices.Equal(ids, want) {
							t.Errorf("child's blockers at limit %d = %v; want %v", limit, ids, want)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				f.start(t, attemptProject, first)
			})
		}
	}
}

// Saved slots are root, child, standalone; topology places standalone first.
func publishQueueTopology(t *testing.T, f *attemptFixture) map[int64]reviewprovider.Pull {
	t.Helper()
	numbers := []int64{2, 4, 6}
	pulls := map[int64]reviewprovider.Pull{}
	for i, n := range numbers {
		p := lifecyclePull(n)
		p.ChangedFiles = 1
		p.IsDraft = new(false)
		p.HeadRepositoryID = 801
		p.HeadRef = []string{"root", "child", "standalone"}[i]
		if n == 4 {
			p.BaseRef = "root"
			p.BaseSHA = pulls[2].HeadSHA
		}
		pulls[n] = p
	}
	repo := reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		pins, err := reviews.PinSourceMetadata(ctx, tx, attemptProject, numbers)
		if err != nil {
			return err
		}
		statuses := []reviews.SourceStatus{}
		for _, n := range numbers {
			statuses = append(statuses, reviews.SourceStatus{Pull: sourceDisplayPull(SourcePlan{Repository: repo, Pull: pulls[n]})})
		}
		_, err = reviews.PublishSourceStatus(ctx, tx, core.Scope{ProjectID: attemptProject}, repo.ID, pins, statuses)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return pulls
}
