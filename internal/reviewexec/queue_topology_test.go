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
			pulls := map[int64]reviewprovider.Pull{}
			for i, n := range []int64{2, 4, 6} {
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
				pins, err := reviews.PinSourceMetadata(ctx, tx, attemptProject, []int64{2, 4, 6})
				if err != nil {
					return err
				}
				statuses := []reviews.SourceStatus{}
				for _, n := range []int64{2, 4, 6} {
					statuses = append(statuses, reviews.SourceStatus{Pull: sourceDisplayPull(SourcePlan{Repository: repo, Pull: pulls[n]})})
				}
				_, err = reviews.PublishSourceStatus(ctx, tx, core.Scope{ProjectID: attemptProject}, repo.ID, pins, statuses)
				return err
			}); err != nil {
				t.Fatal(err)
			}
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
