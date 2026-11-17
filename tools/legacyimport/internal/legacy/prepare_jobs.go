package legacy

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"

	"github.com/benhook1013/FireController/internal/coordimport"
	"github.com/benhook1013/FireController/internal/coordimportmodel"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// JobSelection deliberately selects current snapshots, not a claimed complete
// legacy history. Worker mappings are explicit native identities.
type JobSelection struct {
	Version          int               `json:"version"`
	RepositoryID     int64             `json:"repository_id"`
	ExpectedRevision *int64            `json:"expected_revision"`
	Source           string            `json:"source"`
	Workers          map[string]string `json:"workers"`
	JobIDs           []string          `json:"job_ids"`
	CheckpointMode   string            `json:"checkpoint_mode"`
}

func conversionBlocked() error {
	return &Error{Code: "snapshot_conversion_blocked", Message: "The selected jobs or dependencies cannot be represented without changing source facts; inspect the selection and archive unsupported data separately."}
}

// PrepareJobs returns a native prepared batch only after the sealed source's
// final identity/hash check succeeds. It performs no service or filesystem write.
func PrepareJobs(ctx context.Context, path string, limits Limits, selection JobSelection) (coordimportmodel.Batch, error) {
	if selection.Version != 1 || selection.RepositoryID <= 0 || selection.ExpectedRevision == nil || *selection.ExpectedRevision < 0 || len(selection.JobIDs) < 1 || len(selection.JobIDs) > 25 || len(selection.Workers) > 1000 || (selection.CheckpointMode != "none" && selection.CheckpointMode != "latest" && selection.CheckpointMode != "all") {
		return coordimportmodel.Batch{}, conversionBlocked()
	}
	seen := map[string]bool{}
	for _, id := range selection.JobIDs {
		if id == "" || len(id) > 200 || seen[id] {
			return coordimportmodel.Batch{}, conversionBlocked()
		}
		seen[id] = true
	}
	var batch coordimportmodel.Batch
	_, err := withInspection(ctx, path, limits, func(db *sql.DB, inspection Inspection) error {
		if inspection.Versions.Jobs == 0 {
			return conversionBlocked()
		}
		batch = coordimportmodel.Batch{Version: 1, RepositoryID: selection.RepositoryID, ExpectedRevision: selection.ExpectedRevision, Source: selection.Source, SourceSHA256: inspection.SHA256, Jobs: make([]coordimportmodel.PreparedJob, 0, len(selection.JobIDs))}
		for _, id := range selection.JobIDs {
			var item coordimportmodel.PreparedJob
			var worker string
			item.SourceKey = id
			rows, err := db.QueryContext(ctx, `SELECT j.title,b.brief,j.worker,j.status,j.created_at,j.updated_at,j.revision,
typeof(j.id)||','||typeof(b.job_id)||','||typeof(j.brief_revision)||','||typeof(b.brief_revision)||','||typeof(j.title)||','||typeof(b.brief)||','||typeof(j.worker)||','||typeof(j.status)||','||typeof(j.created_at)||','||typeof(j.updated_at)||','||typeof(j.revision)
FROM jobs j JOIN job_briefs b ON b.job_id=j.id AND b.brief_revision=j.brief_revision WHERE j.id=? LIMIT 2`, id)
			if err != nil {
				return conversionBlocked()
			}
			count := 0
			for rows.Next() {
				count++
				var kinds string
				if err = rows.Scan(&item.Title, &item.Brief, &worker, &item.Status, &item.CreatedAt, &item.UpdatedAt, &item.SourceRevision, &kinds); err != nil {
					break
				}
				if count != 1 || kinds != "text,text,integer,integer,text,text,text,text,text,text,integer" {
					err = conversionBlocked()
					break
				}
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
			if err != nil || count != 1 {
				return conversionBlocked()
			}
			var ok bool
			item.WorkerID, ok = selection.Workers[worker]
			if !ok || item.WorkerID == "" {
				return conversionBlocked()
			}
			item.Checkpoints = []coordimportmodel.PreparedCheckpoint{}
			if selection.CheckpointMode != "none" {
				columns := `sequence,done,next_steps,blocker,pointers_json,created_at,typeof(sequence)||','||typeof(done)||','||typeof(next_steps)||','||typeof(blocker)||','||typeof(pointers_json)||','||typeof(created_at)`
				query := `SELECT ` + columns + ` FROM job_checkpoints WHERE job_id=? ORDER BY sequence ASC LIMIT 101`
				args := []any{id}
				if selection.CheckpointMode == "latest" {
					// Fetch two tied maxima so a weakened source key cannot make
					// latest silently choose one of two contradictory originals.
					query = `SELECT ` + columns + ` FROM job_checkpoints WHERE job_id=? AND sequence=(SELECT max(sequence) FROM job_checkpoints WHERE job_id=?) LIMIT 2`
					args = append(args, id)
				}
				rows, e := db.QueryContext(ctx, query, args...)
				if e != nil {
					return conversionBlocked()
				}
				for rows.Next() {
					var cp coordimportmodel.PreparedCheckpoint
					var sequence int64
					var pointers, kinds string
					if e = rows.Scan(&sequence, &cp.Done, &cp.Next, &cp.Blocker, &pointers, &cp.CreatedAt, &kinds); e != nil {
						break
					}
					if sequence <= 0 || kinds != "integer,text,text,text,text,text" || len(item.Checkpoints) >= 100 || strictjson.Validate([]byte(pointers)) != nil {
						e = conversionBlocked()
						break
					}
					// Legacy checkpoints have no author field. This reserved label
					// reports unavailable attribution; it is not a named old actor.
					cp.SourceKey = strconv.FormatInt(sequence, 10)
					cp.ActorID = "imported-history"
					cp.WorkerID = ""
					cp.Evidence = []string{}
					var refs map[string]json.RawMessage
					if json.Unmarshal([]byte(pointers), &refs) != nil || refs == nil {
						e = conversionBlocked()
						break
					}
					// Pointers are named source fields, not native evidence URIs.
					// Preserve exact names/values as inert JSON in a single ref.
					if len(refs) > 0 {
						cp.Evidence = []string{pointers}
					}
					item.Checkpoints = append(item.Checkpoints, cp)
				}
				if e == nil {
					e = rows.Err()
				}
				rows.Close()
				if e != nil {
					return conversionBlocked()
				}
			}
			batch.Jobs = append(batch.Jobs, item)
		}
		normalized, _, e := coordimport.Normalize(batch)
		if e != nil {
			return conversionBlocked()
		}
		batch = normalized
		return nil
	})
	if err != nil {
		return coordimportmodel.Batch{}, err
	}
	return batch, nil
}
