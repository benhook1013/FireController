package publicsite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/projectmap"
)

type linkedJob struct {
	title, body, status, worker string
}

func linkedSourceUnavailable() error {
	return core.Fail("publication_source_unavailable", "A selected public source or exact revision is unavailable in this project.", 409)
}

// resolveLinkedSources replaces authenticated source references with their
// deliberately selected anonymous fields. The returned manifest contains no
// source IDs or source-revision selections.
func resolveLinkedSources(ctx context.Context, tx *sql.Tx, project string, manifest Manifest) (Manifest, error) {
	resolved, err := NormalizeManifest(manifest)
	if err != nil {
		return Manifest{}, invalid()
	}

	selectedJobs := make([]linkedJob, 0, len(resolved.JobRefs))
	for _, ref := range resolved.JobRefs {
		summary, readErr := jobs.ReadSummaryRevision(ctx, tx, project, ref.JobID, ref.SummaryRevision)
		if readErr != nil || strings.TrimSpace(summary.Title) == "" || strings.TrimSpace(summary.Body) == "" {
			return Manifest{}, linkedSourceUnavailable()
		}
		var current linkedJob
		current.title, current.body = summary.Title, summary.Body
		if err = tx.QueryRowContext(ctx, `SELECT status,COALESCE(worker_id,'') FROM jobs WHERE project_id=? AND id=?`, project, ref.JobID).Scan(&current.status, &current.worker); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Manifest{}, linkedSourceUnavailable()
			}
			return Manifest{}, unavailable()
		}
		selectedJobs = append(selectedJobs, current)
		resolved.Progress = append(resolved.Progress, Card{Key: ref.Key, Title: current.title, Description: current.body, Status: current.status, Links: []Link{}})
	}

	for _, ref := range resolved.MapRefs {
		revision, readErr := projectmap.ReadRevision(ctx, tx, project, ref.EntryID, ref.Revision)
		if readErr != nil {
			return Manifest{}, linkedSourceUnavailable()
		}
		state := revision.State
		status := state.State
		if state.Archived {
			status = "archived"
		}
		resolved.ProjectMap = append(resolved.ProjectMap, Card{Key: ref.Key, Title: state.Title, Description: state.Description, Status: status, Links: []Link{}})
	}

	for _, ref := range resolved.WorkerRefs {
		var exists int
		if err = tx.QueryRowContext(ctx, `SELECT 1 FROM workers WHERE project_id=? AND id=?`, project, ref.WorkerID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Manifest{}, linkedSourceUnavailable()
			}
			return Manifest{}, unavailable()
		}
		lane := WorkerLane{Key: ref.Key, Label: ref.Label, Status: "idle", Milestones: []string{}}
		for _, selected := range selectedJobs {
			if selected.worker != ref.WorkerID {
				continue
			}
			if lane.Headline == "" {
				lane.Headline = selected.title
				lane.Status = selected.status
			}
			if len(lane.Milestones) < MaxMilestones {
				lane.Milestones = append(lane.Milestones, selected.title)
			}
		}
		resolved.WorkerLanes = append(resolved.WorkerLanes, lane)
	}

	resolved.JobRefs = nil
	resolved.MapRefs = nil
	resolved.WorkerRefs = nil
	resolved, err = NormalizeManifest(resolved)
	if err != nil {
		return Manifest{}, unavailable()
	}
	resolved.JobRefs = nil
	resolved.MapRefs = nil
	resolved.WorkerRefs = nil
	// Guard the resolver boundary independently of the eventual Content encoder.
	if data, marshalErr := json.Marshal(resolved); marshalErr != nil || len(data) > MaxManifestBytes {
		return Manifest{}, unavailable()
	}
	return resolved, nil
}
