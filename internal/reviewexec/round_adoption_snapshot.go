package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/benhook1013/FireController/internal/adoptionholds"
	"github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// ValidateRoundAdoptionSnapshot validates immutable configuration provenance.
// It does not turn source dry counts into native capture or completion credit.
func ValidateRoundAdoptionSnapshot(ctx context.Context, q EvidenceQuery) error {
	return validateRoundAdoptions(ctx, q)
}
func validateRoundAdoptions(ctx context.Context, q EvidenceQuery) error {
	var version int
	err := q.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:review_round_adoption'`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		var carried bool
		if err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_round_changes WHERE json_extract(body_json,'$.origin_source') IS NOT NULL)`).Scan(&carried); err != nil {
			return err
		}
		if carried {
			return roundUnavailable()
		}
		return nil
	}
	if err != nil {
		return err
	}
	if version != 1 {
		return roundUnavailable()
	}
	var definition string
	if err = q.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(sql AS BLOB))<=16384 THEN sql END FROM sqlite_schema WHERE type='table' AND name='review_round_adoptions'`).Scan(&definition); err != nil {
		return err
	}
	if strings.TrimSuffix(strings.TrimSpace(definition), ";") != strings.TrimSuffix(strings.TrimSpace(RoundAdoptionSchemaSQL()[0]), ";") {
		return roundUnavailable()
	}
	var objects int
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE tbl_name='review_round_adoptions' AND sql IS NOT NULL`).Scan(&objects); err != nil {
		return err
	}
	if objects != 1 {
		return roundUnavailable()
	}
	var bad bool
	if err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_round_adoptions WHERE typeof(project_id)!='text' OR project_id='' OR length(CAST(project_id AS BLOB))>128 OR typeof(pull_number)!='integer' OR typeof(channel)!='text' OR length(CAST(channel AS BLOB))>128 OR typeof(grant_revision)!='integer' OR typeof(epoch)!='integer' OR typeof(source)!='text' OR length(CAST(source AS BLOB))>200 OR typeof(source_key)!='text' OR length(CAST(source_key AS BLOB))>200 OR typeof(source_sha256)!='text' OR length(CAST(source_sha256 AS BLOB))!=64 OR typeof(prepared_json)!='text' OR length(CAST(prepared_json AS BLOB))>16384)`).Scan(&bad); err != nil {
		return err
	}
	if bad {
		return roundUnavailable()
	}
	if err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_round_changes h LEFT JOIN review_round_adoptions a ON a.project_id=h.project_id AND a.pull_number=h.pull_number AND a.channel=h.channel WHERE json_extract(h.body_json,'$.origin_source') IS NOT NULL AND (a.project_id IS NULL OR h.epoch!=1))`).Scan(&bad); err != nil {
		return err
	}
	if bad {
		return roundUnavailable()
	}
	project, channel := "", ""
	var pull int64
	for {
		rows, err := q.QueryContext(ctx, `SELECT project_id,pull_number,channel,grant_revision,epoch,source,source_key,source_sha256,prepared_json FROM review_round_adoptions WHERE (project_id,pull_number,channel)>(?,?,?) ORDER BY project_id,pull_number,channel LIMIT 64`, project, pull, channel)
		if err != nil {
			return err
		}
		type record struct {
			project, channel, source, key, sha, body string
			pull, grant, epoch                       int64
		}
		page := []record{}
		for rows.Next() {
			var r record
			if err = rows.Scan(&r.project, &r.pull, &r.channel, &r.grant, &r.epoch, &r.source, &r.key, &r.sha, &r.body); err != nil {
				break
			}
			page = append(page, r)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, r := range page {
			p, err := decodeRoundAdoption([]byte(r.body))
			if err != nil {
				return roundUnavailable()
			}
			_, body, err := normalizeRoundAdoption(p)
			if err != nil || string(body) != r.body || !roundScope(r.project, r.pull, r.channel) || p.PullNumber != r.pull || p.Channel != r.channel || p.Source != r.source || p.SourceKey != r.key || p.SourceSHA256 != r.sha || r.grant != 1 || r.epoch != 1 {
				return roundUnavailable()
			}
			var repository int64
			if err = q.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, r.project).Scan(&repository); err != nil {
				return err
			}
			if repository != p.RepositoryID {
				return roundUnavailable()
			}
			if p.Obligations != nil {
				ref := p.Obligations
				if err := adoptionholds.VerifyRetainedSource(ctx, q, r.project, adoptionholdsmodel.Source{Namespace: ref.Namespace, RepositoryID: p.RepositoryID, SnapshotSHA256: ref.SnapshotSHA256}, ref.Revision); err != nil {
					return roundUnavailable()
				}
			}
			var action, baseline string
			if err = q.QueryRowContext(ctx, `SELECT action,body_json FROM review_round_changes WHERE project_id=? AND pull_number=? AND channel=? AND revision=1 AND epoch=1`, r.project, r.pull, r.channel).Scan(&action, &baseline); err != nil {
				return err
			}
			var c RoundControl
			if strictjson.Decode([]byte(baseline), &c) != nil || encoded(c) != baseline || !validRoundControl(c) || action != "grant" || c.ProjectID != r.project || c.PullNumber != r.pull || c.Channel != r.channel || c.Revision != 1 || c.Epoch != 1 || c.Used != 0 || c.InitialDry != p.InitialDry || c.Dry != p.InitialDry || c.InitiallyTapered != p.Tapered || c.Tapered != (p.Tapered && p.MinimumRemaining == 0) || c.Closed != p.Closed || c.Minimum != p.MinimumRemaining || c.Maximum != p.MaximumRemaining || c.MaximumUnbounded != p.MaximumUnbounded || c.Taper != p.Taper || c.OriginSource != p.Source || c.OriginKey != p.SourceKey || c.OriginSHA256 != p.SourceSHA256 {
				return roundUnavailable()
			}
			project, pull, channel = r.project, r.pull, r.channel
		}
		if len(page) < 64 {
			return nil
		}
	}
}
