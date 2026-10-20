package adoptionholds

import (
	"context"
	"database/sql"
	"errors"
	m "github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
	"slices"
	"time"
)

type Query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}
type Facts struct {
	Open int64 `json:"open"`
}

func validTime(s string) bool {
	v, e := time.Parse(time.RFC3339Nano, s)
	return e == nil && !v.IsZero() && len(s) <= 64
}
func ReadSource(ctx context.Context, q Query, project, namespace string) (m.SourceState, error) {
	var out m.SourceState
	present, err := domainPresent(ctx, q)
	if err != nil {
		return out, unavailable()
	}
	if !present {
		return out, core.Fail("not_found", "The retained source is unavailable in this project.", 404)
	}
	out.ProjectID = project
	out.Source.Namespace = namespace
	if !identifier.MatchString(project) || !sourceKey(namespace) {
		return out, invalid()
	}
	err = q.QueryRowContext(ctx, `SELECT repository_id,snapshot_sha256,revision FROM review_adoption_sources WHERE project_id=? AND namespace=?`, project, namespace).Scan(&out.Source.RepositoryID, &out.Source.SnapshotSHA256, &out.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return out, core.Fail("not_found", "The retained source is unavailable in this project.", 404)
	}
	if err != nil {
		return out, err
	}
	if !validSource(out.Source) || out.Revision < 1 {
		return out, unavailable()
	}
	var repository int64
	if err = q.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repository); err != nil {
		return out, err
	}
	if repository != out.Source.RepositoryID {
		return out, core.Fail("repository_changed", "The retained source belongs to a different repository.", 409)
	}
	return out, nil
}
func VerifySource(ctx context.Context, q Query, project string, source m.Source, revision int64) error {
	state, err := ReadSource(ctx, q, project, source.Namespace)
	if err != nil {
		return err
	}
	if state.Source != source || state.Revision != revision {
		return conflict()
	}
	return nil
}

// VerifyRetainedSource validates immutable policy provenance after later
// additive imports. VerifySource additionally pins the current admission epoch.
func VerifyRetainedSource(ctx context.Context, q Query, project string, source m.Source, revision int64) error {
	if revision < 1 || !validSource(source) {
		return invalid()
	}
	state, err := ReadSource(ctx, q, project, source.Namespace)
	if err != nil {
		return err
	}
	if state.Source != source || revision > state.Revision {
		return conflict()
	}
	var body, sha string
	if err = q.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(batch_json AS BLOB))<=262144 THEN batch_json END,batch_sha256 FROM review_adoption_imports WHERE project_id=? AND namespace=? AND revision=?`, project, source.Namespace, revision).Scan(&body, &sha); err != nil {
		return unavailable()
	}
	var batch m.Batch
	if digest([]byte(body)) != sha || strictjson.Decode([]byte(body), &batch) != nil {
		return unavailable()
	}
	normalized, canonical, err := NormalizeBatch(batch)
	if err != nil || string(canonical) != body || normalized.Source != source || *normalized.ExpectedRevision != revision-1 {
		return unavailable()
	}
	return nil
}
func readDetail(ctx context.Context, q Query, project, id string) (m.Detail, error) {
	return readDetailWithSources(ctx, q, project, id, nil)
}

// A facts read may reuse successfully validated namespaces within its current
// transaction. Originals, target lists and resolutions remain individually checked.
func readDetailWithSources(ctx context.Context, q Query, project, id string, sources map[string]m.SourceState) (m.Detail, error) {
	var d m.Detail
	var namespace, body, sha string
	err := q.QueryRowContext(ctx, `SELECT id,project_id,namespace,source_revision,CASE WHEN length(CAST(original_json AS BLOB))<=65536 THEN original_json END,original_sha256,status,revision,actor_id,worker_id,created_at FROM review_adoption_obligations WHERE project_id=? AND id=?`, project, id).Scan(&d.ID, &d.ProjectID, &namespace, &d.SourceRevision, &body, &sha, &d.Status, &d.Revision, &d.ActorID, &d.WorkerID, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return d, core.Fail("not_found", "The obligation is unavailable in this project.", 404)
	}
	if err != nil {
		return d, err
	}
	if !identifier.MatchString(d.ID) || !identifier.MatchString(d.ActorID) || !identifier.MatchString(d.WorkerID) || !validTime(d.CreatedAt) || digest([]byte(body)) != sha || strictjson.Decode([]byte(body), &d.Original) != nil {
		return d, unavailable()
	}
	normalized, err := NormalizeObligation(d.Original)
	if err != nil || string(encode(normalized)) != body {
		return d, unavailable()
	}
	state, found := sources[namespace]
	if !found {
		state, err = ReadSource(ctx, q, project, namespace)
		if err != nil {
			return d, retainedSourceReadError(err)
		}
		// Bound per-read memory; overflow namespaces are revalidated normally.
		if sources != nil && len(sources) < 64 {
			sources[namespace] = state
		}
	}
	d.Source = state.Source
	if d.SourceRevision < 1 || d.SourceRevision > state.Revision {
		return d, unavailable()
	}
	var key string
	if err = q.QueryRowContext(ctx, `SELECT source_key FROM review_adoption_obligations WHERE id=?`, id).Scan(&key); err != nil || key != d.Original.SourceKey {
		return d, unavailable()
	}
	rows, err := q.QueryContext(ctx, `SELECT project_id,pull_number FROM review_adoption_targets WHERE obligation_id=? ORDER BY pull_number`, id)
	if err != nil {
		return d, err
	}
	numbers := []int64{}
	for rows.Next() {
		var owner string
		var n int64
		if err = rows.Scan(&owner, &n); err != nil || owner != project {
			rows.Close()
			return d, unavailable()
		}
		numbers = append(numbers, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return d, err
	}
	if !slices.Equal(numbers, d.Original.AffectedPullNumbers) {
		return d, unavailable()
	}
	var resolution m.Resolution
	var refs string
	err = q.QueryRowContext(ctx, `SELECT revision,reason,evidence_json,actor_id,worker_id,resolved_at FROM review_adoption_resolutions WHERE obligation_id=?`, id).Scan(&resolution.Revision, &resolution.Reason, &refs, &resolution.ActorID, &resolution.WorkerID, &resolution.ResolvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		if d.Status != "open" || d.Revision != 1 {
			return d, unavailable()
		}
		return d, nil
	}
	if err != nil {
		return d, err
	}
	if d.Status != "resolved" || d.Revision != 2 || resolution.Revision != 2 || !text(resolution.Reason, 4096) || !identifier.MatchString(resolution.ActorID) || !identifier.MatchString(resolution.WorkerID) || !validTime(resolution.ResolvedAt) || strictjson.Decode([]byte(refs), &resolution.Evidence) != nil || !m.ValidResolutionEvidence(resolution.Evidence) || string(encode(resolution.Evidence)) != refs {
		return d, unavailable()
	}
	previous := resolution.Revision - 1
	if _, err := m.NormalizeResolution(m.ResolveRequest{ExpectedRevision: &previous, Reason: resolution.Reason, Evidence: resolution.Evidence}); err != nil {
		return d, unavailable()
	}
	d.Resolution = &resolution
	return d, nil
}

// ReadFacts reads only the selected project/PR using its target index. Original
// records are validated in bounded pages; no captured credit is consulted.
func readFacts(ctx context.Context, q Query, project string, number int64) (Facts, error) {
	var facts Facts
	if !identifier.MatchString(project) || !pull(number) {
		return facts, invalid()
	}
	after := ""
	sources := map[string]m.SourceState{}
	for {
		rows, err := q.QueryContext(ctx, `SELECT obligation_id FROM review_adoption_targets WHERE project_id=? AND pull_number=? AND obligation_id>? ORDER BY obligation_id LIMIT 64`, project, number, after)
		if err != nil {
			return facts, err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return facts, err
		}
		for _, id := range ids {
			d, err := readDetailWithSources(ctx, q, project, id, sources)
			if err != nil {
				return facts, retainedSourceReadError(err)
			}
			if d.Status == "open" {
				facts.Open++
				if facts.Open > 1<<53-1 {
					return facts, unavailable()
				}
			}
			after = id
		}
		if len(ids) < 64 {
			return facts, nil
		}
	}
}

// ReadFacts treats only a genuinely absent optional domain as empty.
func ReadFacts(ctx context.Context, q Query, project string, number int64) (Facts, error) {
	if !identifier.MatchString(project) || !pull(number) {
		return Facts{}, invalid()
	}
	present, err := domainPresent(ctx, q)
	if err != nil {
		return Facts{}, unavailable()
	}
	if !present {
		return Facts{}, nil
	}
	facts, err := readFacts(ctx, q, project, number)
	if err != nil {
		return Facts{}, retainedSourceReadError(err)
	}
	return facts, nil
}

// Only the proven repository binding mismatch is a protective conflict.
// Missing or malformed retained records and ordinary SQL failures stay unavailable.
func retainedSourceReadError(err error) error {
	var failure *core.Error
	if errors.As(err, &failure) && failure.Code == "repository_changed" {
		return err
	}
	return unavailable()
}
func HasOpen(ctx context.Context, q Query, project string, number int64) (bool, error) {
	facts, err := ReadFacts(ctx, q, project, number)
	return facts.Open > 0, err
}
func (s *Service) Detail(ctx context.Context, scope core.Scope, id string) (m.Detail, error) {
	var out m.Detail
	if !identifier.MatchString(id) {
		return out, invalid()
	}
	err := s.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		out, err = readDetail(ctx, tx, scope.ProjectID, id)
		return err
	})
	return out, err
}
func (s *Service) List(ctx context.Context, scope core.Scope, number int64, status string, limit int, cursor string) (m.Page, error) {
	out := m.Page{Items: []m.ListItem{}}
	if number != 0 && !pull(number) || (status != "all" && status != "open" && status != "resolved") || limit < 1 || limit > 100 {
		return out, invalid()
	}
	filter := string(encode([]any{"adoption-obligations:v1", number, status, limit}))
	after, err := s.store.ParseCursor(scope, filter, cursor)
	if err != nil {
		return out, err
	}
	if after != "" && !identifier.MatchString(after) {
		return out, invalid()
	}
	err = s.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT o.id FROM review_adoption_obligations o WHERE o.project_id=? AND o.id>? AND (?='all' OR o.status=?) AND (?=0 OR EXISTS(SELECT 1 FROM review_adoption_targets t WHERE t.obligation_id=o.id AND t.project_id=o.project_id AND t.pull_number=?)) ORDER BY o.id LIMIT ?`, scope.ProjectID, after, status, status, number, number, limit+1)
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for i, id := range ids {
			if i == limit {
				out.NextCursor = s.store.Cursor(scope, filter, ids[i-1])
				break
			}
			d, err := readDetail(ctx, tx, scope.ProjectID, id)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, m.ListItem{ID: d.ID, Namespace: d.Source.Namespace, SourceKey: d.Original.SourceKey, Kind: d.Original.Kind, Status: d.Status, Revision: d.Revision, AffectedPullNumbers: d.Original.AffectedPullNumbers})
		}
		return nil
	})
	return out, err
}
