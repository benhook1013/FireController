package jobs_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
)

func preparedJobSnapshot() jobs.PreparedJob {
	return jobs.PreparedJob{
		SourceKey: " original source key ", SourceRevision: 42,
		Title: "Historical job 雪", Brief: "  Original brief\n\twith space  ", Status: "parked",
		CreatedAt: "2024-02-03T04:05:06.123456789+13:00", UpdatedAt: "2024-03-04T05:06:07Z",
		Checkpoints: []jobs.PreparedCheckpoint{
			{SourceKey: "checkpoint one", ActorID: "original-actor", Done: "  Original done 雪  ", Next: "next\nstep",
				Blocker: "  blocker  ", Evidence: []string{"ref-two", "ref-one"}, CreatedAt: "2024-03-04T05:06:07+13:00"},
			{SourceKey: "checkpoint two", ActorID: "another-actor", Done: "second", Next: "continue", CreatedAt: "2024-02-04T05:06:07Z"},
		},
	}
}

func TestPreparedJobNormalizationPreservesHistoricalBytes(t *testing.T) {
	original := preparedJobSnapshot()
	normalized, err := jobs.NormalizePreparedJob(original)
	if err != nil {
		t.Fatal(err)
	}
	want := preparedJobSnapshot()
	want.Checkpoints[1].Evidence = []string{}
	if !reflect.DeepEqual(normalized, want) {
		t.Fatal("normalization rewrote historical content")
	}
	normalized.Checkpoints[0].Evidence[0] = "changed"
	if original.Checkpoints[0].Evidence[0] != "ref-two" || original.Checkpoints[1].Evidence != nil {
		t.Fatal("normalization mutated caller-owned collections")
	}
	original.Checkpoints = nil
	value, err := jobs.NormalizePreparedJob(original)
	if err != nil || value.Checkpoints == nil || len(value.Checkpoints) != 0 {
		t.Fatal("absent checkpoints were not normalized to an empty collection")
	}
	// Native byte limits, not rune counts, govern admission.
	original.SourceKey = strings.Repeat("k", 200)
	original.Title = strings.Repeat("t", 240)
	original.Brief = strings.Repeat("b", 32768)
	if _, err := jobs.NormalizePreparedJob(original); err != nil {
		t.Fatal("exact native byte bounds refused:", err)
	}
	original.Checkpoints = make([]jobs.PreparedCheckpoint, 100)
	for i := range original.Checkpoints {
		original.Checkpoints[i] = jobs.PreparedCheckpoint{SourceKey: strconv.Itoa(i), ActorID: "historical-actor", Done: "done", Next: "next", CreatedAt: original.UpdatedAt}
	}
	if value, err := jobs.NormalizePreparedJob(original); err != nil || len(value.Checkpoints) != 100 {
		t.Fatal("exact checkpoint count refused:", err)
	}
}

func TestPreparedJobNormalizationRefusesInvalidSnapshots(t *testing.T) {
	tests := map[string]func(*jobs.PreparedJob){
		"blank source":      func(v *jobs.PreparedJob) { v.SourceKey = " \t\n" },
		"source bytes":      func(v *jobs.PreparedJob) { v.SourceKey = strings.Repeat("雪", 67) },
		"source utf8":       func(v *jobs.PreparedJob) { v.SourceKey = string([]byte{255}) },
		"source revision":   func(v *jobs.PreparedJob) { v.SourceRevision = 0 },
		"padded title":      func(v *jobs.PreparedJob) { v.Title = " padded " },
		"empty title":       func(v *jobs.PreparedJob) { v.Title = "" },
		"title utf8":        func(v *jobs.PreparedJob) { v.Title = string([]byte{255}) },
		"title bytes":       func(v *jobs.PreparedJob) { v.Title = strings.Repeat("雪", 81) },
		"brief utf8":        func(v *jobs.PreparedJob) { v.Brief = string([]byte{255}) },
		"brief bytes":       func(v *jobs.PreparedJob) { v.Brief = strings.Repeat("雪", 10923) },
		"status":            func(v *jobs.PreparedJob) { v.Status = "unknown" },
		"job worker":        func(v *jobs.PreparedJob) { v.WorkerID = "../worker" },
		"zero created":      func(v *jobs.PreparedJob) { v.CreatedAt = "0001-01-01T00:00:00Z" },
		"missing updated":   func(v *jobs.PreparedJob) { v.UpdatedAt = "" },
		"invalid updated":   func(v *jobs.PreparedJob) { v.UpdatedAt = "yesterday" },
		"long time":         func(v *jobs.PreparedJob) { v.CreatedAt = "2024-02-03T04:05:06." + strings.Repeat("1", 64) + "Z" },
		"checkpoint source": func(v *jobs.PreparedJob) { v.Checkpoints[0].SourceKey = " " },
		"duplicate source":  func(v *jobs.PreparedJob) { v.Checkpoints[1].SourceKey = v.Checkpoints[0].SourceKey },
		"actor":             func(v *jobs.PreparedJob) { v.Checkpoints[0].ActorID = "" },
		"checkpoint worker": func(v *jobs.PreparedJob) { v.Checkpoints[0].WorkerID = "bad worker" },
		"checkpoint time":   func(v *jobs.PreparedJob) { v.Checkpoints[0].CreatedAt = "" },
		"blank done":        func(v *jobs.PreparedJob) { v.Checkpoints[0].Done = "\t" },
		"control next":      func(v *jobs.PreparedJob) { v.Checkpoints[0].Next = "next\x00" },
		"blocker bytes":     func(v *jobs.PreparedJob) { v.Checkpoints[0].Blocker = strings.Repeat("b", 4097) },
		"blank evidence":    func(v *jobs.PreparedJob) { v.Checkpoints[0].Evidence = []string{" "} },
		"checkpoint count":  func(v *jobs.PreparedJob) { v.Checkpoints = make([]jobs.PreparedCheckpoint, 101) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := preparedJobSnapshot()
			mutate(&value)
			if _, err := jobs.NormalizePreparedJob(value); err == nil {
				t.Fatal("invalid snapshot admitted")
			}
		})
	}
}

func TestPreparedJobInsertPreservesNativeReadsWithoutReplayEffects(t *testing.T) {
	a := setupAPI(t)
	project := a.createProject("prepared-jobs", "example/prepared-jobs")
	worker := a.createWorker(project, "builder")
	if err := inbox.EnsureSchema(t.Context(), a.store); err != nil {
		t.Fatal(err)
	}
	inbox.Register(a.mux, a.store)
	before := contextState(t, a)
	item := preparedJobSnapshot()
	item.WorkerID = worker
	item.Checkpoints[0].WorkerID = worker
	tx, err := a.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := jobs.InsertPreparedJob(t.Context(), tx, project, "imported-job", item, []string{"imported-checkpoint-one", "imported-checkpoint-two"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/projects/" + project + "/jobs/imported-job"
	detail := a.call("GET", base, a.admin, "", nil)
	progressOK(t, detail, 200)
	var current map[string]any
	decodeResponse(t, detail, &current)
	if current["revision"] != float64(1) || current["title"] != item.Title || current["brief"] != item.Brief ||
		current["created_at"] != item.CreatedAt || current["updated_at"] != item.UpdatedAt || current["worker_id"] != worker || current["status"] != item.Status {
		t.Fatal("native detail did not preserve selected state")
	}
	if _, exists := current["source_revision"]; exists {
		t.Fatal("source revision leaked into native state")
	}
	history := a.call("GET", base+"/history", a.admin, "", nil)
	progressOK(t, history, 200)
	var revisions struct {
		Items []struct {
			Revision int64          `json:"revision"`
			Actor    string         `json:"actor_id"`
			At       string         `json:"created_at"`
			State    map[string]any `json:"state"`
		} `json:"items"`
	}
	decodeResponse(t, history, &revisions)
	if len(revisions.Items) != 1 || revisions.Items[0].Revision != 1 || revisions.Items[0].Actor != "imported-history" ||
		revisions.Items[0].At != item.UpdatedAt || !reflect.DeepEqual(revisions.Items[0].State, current) {
		t.Fatal("native history fabricated revisions, attribution, or state")
	}
	archive := a.call("GET", base+"/checkpoints", a.admin, "", nil)
	progressOK(t, archive, 200)
	var checkpoints struct {
		Items []checkpointAPIValue `json:"items"`
	}
	decodeResponse(t, archive, &checkpoints)
	if len(checkpoints.Items) != 2 {
		t.Fatal("checkpoint count")
	}
	for i, checkpoint := range checkpoints.Items {
		original := item.Checkpoints[1-i]
		if checkpoint.JobRevision != 1 || checkpoint.ActorID != original.ActorID || checkpoint.WorkerID != original.WorkerID ||
			checkpoint.Done != original.Done || checkpoint.Next != original.Next || checkpoint.Blocker != original.Blocker || checkpoint.CreatedAt != original.CreatedAt ||
			!reflect.DeepEqual(checkpoint.Evidence, append([]string{}, original.Evidence...)) {
			t.Fatal("checkpoint content, attribution, time, or order changed")
		}
	}
	if checkpoints.Items[0].Sequence <= checkpoints.Items[1].Sequence {
		t.Fatal("input order did not govern native sequence")
	}
	context := contextRead(t, a, base, a.admin, worker)
	if context.Job.Revision != 1 || context.Checkpoint == nil || context.Checkpoint.ID != "imported-checkpoint-two" || !context.Complete {
		t.Fatal("native context cannot read imported state")
	}
	after := contextState(t, a)
	for _, table := range []string{"jobs", "job_history", "job_checkpoints"} {
		delete(before, table)
		delete(after, table)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("import/read created events, receipts, deliveries or other domain state")
	}
}

func TestPreparedJobInsertValidationAndCallerRollback(t *testing.T) {
	a := setupAPI(t)
	project := a.createProject("prepared-rollback", "example/prepared-rollback")
	otherProject := a.createProject("prepared-other", "example/prepared-other")
	foreignWorker := a.createWorker(otherProject, "foreign")
	item := preparedJobSnapshot()
	for name, tc := range map[string]struct {
		project, id string
		ids         []string
		mutate      func(*jobs.PreparedJob)
	}{
		"project":                   {"bad/project", "new-job", []string{"one", "two"}, nil},
		"job":                       {project, "bad job", []string{"one", "two"}, nil},
		"checkpoint count":          {project, "new-job", []string{"one"}, nil},
		"checkpoint id":             {project, "new-job", []string{"one", "bad/id"}, nil},
		"duplicate id":              {project, "new-job", []string{"one", "one"}, nil},
		"foreign job worker":        {project, "new-job", []string{"one", "two"}, func(v *jobs.PreparedJob) { v.WorkerID = foreignWorker }},
		"foreign checkpoint worker": {project, "new-job", []string{"one", "two"}, func(v *jobs.PreparedJob) { v.Checkpoints[1].WorkerID = foreignWorker }},
		"invalid snapshot":          {project, "new-job", []string{"one", "two"}, func(v *jobs.PreparedJob) { v.Title = "" }},
	} {
		t.Run(name, func(t *testing.T) {
			before := progressState(t, a)
			value := preparedJobSnapshot()
			if tc.mutate != nil {
				tc.mutate(&value)
			}
			tx, err := a.store.DB().BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err := jobs.InsertPreparedJob(t.Context(), tx, tc.project, tc.id, value, tc.ids); err == nil {
				t.Fatal("invalid insert succeeded")
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, progressState(t, a)) {
				t.Fatal("refused import changed state")
			}
		})
	}
	// A global checkpoint ID collision can occur after the new job, history,
	// and first checkpoint were inserted. Only the caller owns rollback.
	tx, err := a.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := jobs.InsertPreparedJob(t.Context(), tx, project, "existing-job", item, []string{"existing-one", "existing-two"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	before := progressState(t, a)
	tx, err = a.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := jobs.InsertPreparedJob(t.Context(), tx, project, "rolled-back-job", item, []string{"new-one", "existing-two"}); err == nil {
		t.Fatal("checkpoint collision admitted")
	}
	var partial int
	if err := tx.QueryRow("SELECT count(*) FROM job_checkpoints WHERE job_id='rolled-back-job'").Scan(&partial); err != nil || partial != 1 {
		t.Fatal("fixture did not reach midway insert failure", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, progressState(t, a)) {
		t.Fatal("caller rollback retained partial import")
	}
	tx, err = a.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := jobs.InsertPreparedJob(t.Context(), tx, project, "existing-job", item, []string{"new-one", "new-two"}); err == nil {
		t.Fatal("existing job overwritten")
	}
	_ = tx.Rollback()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	tx, err = a.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := jobs.InsertPreparedJob(ctx, tx, project, "canceled-job", item, []string{"new-one", "new-two"}); err == nil {
		t.Fatal("canceled import succeeded")
	}
	_ = tx.Rollback()
	if !reflect.DeepEqual(before, progressState(t, a)) {
		t.Fatal("cancellation/collision changed retained state")
	}
	if err := jobs.InsertPreparedJob(t.Context(), nil, project, "job", item, []string{"one", "two"}); err == nil {
		t.Fatal("nil transaction admitted")
	}
	// The prepared wire shape preserves provenance and explicit empty arrays.
	normalized, err := jobs.NormalizePreparedJob(item)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(normalized)
	if err != nil || !strings.Contains(string(data), `"source_revision":42`) || !strings.Contains(string(data), `"evidence":[]`) {
		t.Fatal("prepared wire shape", err)
	}
}
