package publicsite

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"time"

	"github.com/benhook1013/FireController/internal/publicsitemodel"
)

var publicSaltPattern = regexp.MustCompile(`^public_salt_[0-9a-f]{32}$`)
var publicGenerationPattern = regexp.MustCompile(`^public_generation_[0-9a-f]{32}$`)

func manifestForContent(content Content) Manifest {
	return publicsitemodel.EditorialManifest(content)
}

// decodeSnapshotContent validates the same explicit public model used to build
// a publication. Historical summaries are checked against their own captured
// editorial values, never current private facts or later approved revisions.
func decodeSnapshotContent(raw string) (Content, error) {
	validated, err := publicsitemodel.DecodeContent([]byte(raw))
	if err != nil {
		return Content{}, unavailable()
	}
	return validated, nil
}

func validPublicationTime(value string) bool {
	at, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && !at.IsZero() && at.Year() >= 1 && at.Year() <= 9999 && at.UTC().Format(time.RFC3339Nano) == value
}

// ValidateSnapshot inspects retained settings and public projections in the
// caller's read-only transaction. It does not migrate, publish, rehash private
// source observations or fabricate missing public history.
func ValidateSnapshot(ctx context.Context, tx *sql.Tx) error {
	var malformed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM project_publications WHERE length(CAST(project_id AS BLOB))>128 OR length(CAST(slug AS BLOB))>64 OR length(CAST(round_salt AS BLOB))<>44 OR length(CAST(generation AS BLOB)) NOT IN (0,50) OR length(CAST(public_updated_at AS BLOB))>64 OR length(CAST(refresh_error AS BLOB))>64)`).Scan(&malformed); err != nil {
		return err
	}
	if malformed {
		return unavailable()
	}
	rows, err := tx.QueryContext(ctx, `SELECT project_id FROM project_publications ORDER BY project_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			return err
		}
		var project string
		if err = rows.Scan(&project); err != nil {
			return err
		}
		r, found, err := readRecord(ctx, tx, project)
		if err != nil || !found || r.revision < 1 || !publicSaltPattern.MatchString(r.salt) || r.refreshError != "" && r.refreshError != "projection_unavailable" {
			return unavailable()
		}
		manifest, err := DecodeManifest([]byte(r.manifest))
		if err != nil || manifest.Slug != r.slug {
			return unavailable()
		}
		canonical, err := json.Marshal(manifest)
		if err != nil || string(canonical) != r.manifest {
			return unavailable()
		}
		if r.publicRevision == 0 {
			if r.enabled || r.generation != "" || r.updated != "" || r.refreshError != "" {
				return unavailable()
			}
		} else if r.publicRevision < 0 || !publicGenerationPattern.MatchString(r.generation) || !validPublicationTime(r.updated) {
			return unavailable()
		}
		if err = validatePublicHistory(ctx, tx, r); err != nil {
			return err
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	var orphans bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM project_publications p LEFT JOIN projects project ON project.id=p.project_id WHERE project.id IS NULL) OR EXISTS(SELECT 1 FROM public_project_snapshots s LEFT JOIN project_publications p ON p.project_id=s.project_id WHERE p.project_id IS NULL)`).Scan(&orphans); err != nil {
		return err
	}
	if orphans {
		return unavailable()
	}
	return nil
}

func validatePublicHistory(ctx context.Context, tx *sql.Tx, r record) error {
	rows, err := tx.QueryContext(ctx, `SELECT CASE WHEN length(CAST(generation AS BLOB))<=50 THEN generation ELSE NULL END,revision,CASE WHEN length(CAST(content_json AS BLOB))<=262144 THEN content_json ELSE NULL END,CASE WHEN length(CAST(created_at AS BLOB))<=64 THEN created_at ELSE NULL END FROM public_project_snapshots WHERE project_id=? ORDER BY generation,revision LIMIT 1001`, r.project)
	if err != nil {
		return err
	}
	defer rows.Close()
	var count int
	var last int64
	var previous time.Time
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			return err
		}
		count++
		var generation, raw, at string
		var revision int64
		if err = rows.Scan(&generation, &revision, &raw, &at); err != nil {
			return unavailable()
		}
		if count > 1000 || generation != r.generation || revision < 1 || revision <= last || revision > r.publicRevision || !validPublicationTime(at) {
			return unavailable()
		}
		observed, _ := time.Parse(time.RFC3339Nano, at)
		if observed.Before(previous) {
			return unavailable()
		}
		content, err := decodeSnapshotContent(raw)
		if err != nil {
			return err
		}
		if revision == r.publicRevision {
			if at != r.updated {
				return unavailable()
			}
			if r.enabled {
				manifest, err := json.Marshal(manifestForContent(content))
				if err != nil || string(manifest) != r.manifest {
					return unavailable()
				}
			}
		}
		last, previous = revision, observed
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if last != r.publicRevision || r.publicRevision == 0 && count != 0 {
		return unavailable()
	}
	return nil
}
