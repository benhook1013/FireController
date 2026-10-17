package publicsite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

type SourceReader func(context.Context, *sql.Tx, string, Manifest, string) ([]ReviewFacts, error)

type Service struct {
	store   *core.Store
	reader  SourceReader
	streams atomic.Int64
}

func New(store *core.Store, reader SourceReader) *Service {
	return &Service{store: store, reader: reader}
}

type Approved struct {
	ExpectedRevision int64    `json:"expected_revision"`
	Enabled          bool     `json:"enabled"`
	Manifest         Manifest `json:"manifest"`
}

type Configuration struct {
	Revision     int64     `json:"revision"`
	Enabled      bool      `json:"enabled"`
	Manifest     *Manifest `json:"manifest"`
	RefreshError string    `json:"refresh_error"`
}

type Preview struct {
	ExpectedRevision int64    `json:"expected_revision"`
	Enabled          bool     `json:"enabled"`
	Manifest         Manifest `json:"manifest"`
	Content          Content  `json:"content"`
	SHA256           string   `json:"preview_sha256"`
}

type Receipt struct {
	Revision int64  `json:"revision"`
	Outcome  string `json:"outcome"`
}

type Snapshot struct {
	Slug        string  `json:"slug"`
	Generation  string  `json:"generation"`
	Revision    int64   `json:"revision"`
	PublishedAt string  `json:"published_at"`
	Cursor      string  `json:"cursor"`
	Content     Content `json:"content"`
}

type record struct {
	project, slug, manifest, salt, generation, updated, refreshError string
	revision, publicRevision                                         int64
	enabled                                                          bool
}

func invalid() error {
	return core.Fail("validation_failed", "Use a bounded, approved public-project manifest and revision.", 400)
}
func unavailable() error {
	return core.Fail("publication_unavailable", "The stored project publication is unavailable.", 503)
}
func notFound() error { return core.Fail("not_found", "The public project is unavailable.", 404) }
func resync() error {
	return core.Fail("resync_required", "The public cursor is unavailable; read the current public project again.", 409)
}
func conflict(rev int64) error {
	return &core.Error{Code: "publication_revision_conflict", Message: "The project publication changed; read its current revision.", Status: 409, Details: map[string]int64{"current_revision": rev}}
}

func readRecord(ctx context.Context, tx *sql.Tx, project string) (record, bool, error) {
	r := record{project: project}
	err := tx.QueryRowContext(ctx, `SELECT revision,slug,enabled,CASE WHEN length(CAST(manifest_json AS BLOB))<=262144 THEN manifest_json ELSE NULL END,round_salt,generation,public_revision,public_updated_at,refresh_error FROM project_publications WHERE project_id=?`, project).Scan(&r.revision, &r.slug, &r.enabled, &r.manifest, &r.salt, &r.generation, &r.publicRevision, &r.updated, &r.refreshError)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return record{}, false, unavailable()
	}
	return r, true, nil
}

func approved(input Approved) (Approved, error) {
	if input.ExpectedRevision < 0 || input.ExpectedRevision >= math.MaxInt64 {
		return Approved{}, invalid()
	}
	m, err := NormalizeManifest(input.Manifest)
	if err != nil {
		return Approved{}, invalid()
	}
	input.Manifest = m
	// Preview must admit an input that fits its eventual normalized PUT wrapper,
	// including the required hash; an accepted preview cannot be impossible to
	// apply merely because that last field crosses the HTTP request bound.
	data, err := json.Marshal(struct {
		Approved
		PreviewSHA256 string `json:"preview_sha256"`
	}{input, strings.Repeat("0", 64)})
	if err != nil || len(data) > 256<<10 {
		return Approved{}, invalid()
	}
	return input, nil
}

func (s *Service) previewHash(scope core.Scope, input Approved) string {
	data, _ := json.Marshal(struct {
		Domain, Instance, Project, Principal, Worker string
		Input                                        Approved
	}{"firecontroller/public-site/preview/v1", s.store.InstanceID(), scope.ProjectID, scope.PrincipalID, scope.WorkerID, input})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func build(ctx context.Context, tx *sql.Tx, project string, m Manifest, salt string, reader SourceReader, preserve bool) (Content, string, error) {
	var facts []ReviewFacts
	var err error
	if reader != nil {
		facts, err = reader(ctx, tx, project, m, salt)
		if err != nil {
			return Content{}, "", unavailable()
		}
	}
	if preserve {
		facts, err = preserveReviewFacts(ctx, tx, project, m, facts)
		if err != nil {
			return Content{}, "", err
		}
	}
	content, err := BuildWindowContent(m, facts)
	if err != nil {
		return Content{}, "", unavailable()
	}
	data, err := json.Marshal(content)
	if err != nil || len(data) > MaxContentBytes {
		return Content{}, "", unavailable()
	}
	return content, string(data), nil
}

func preserveReviewFacts(ctx context.Context, tx *sql.Tx, project string, m Manifest, incoming []ReviewFacts) ([]ReviewFacts, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(s.content_json AS BLOB))<=262144 THEN s.content_json ELSE NULL END FROM public_project_snapshots s JOIN project_publications p ON p.project_id=s.project_id AND p.generation=s.generation AND p.public_revision=s.revision WHERE p.project_id=? AND p.enabled=1`, project).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return incoming, nil
	}
	if err != nil {
		return nil, unavailable()
	}
	prior, err := decodeSnapshotContent(raw)
	if err != nil {
		return nil, err
	}
	current := map[int64]ReviewFacts{}
	previous := map[int64]PublicReview{}
	for _, fact := range incoming {
		if _, exists := current[fact.Number]; exists {
			return nil, unavailable()
		}
		current[fact.Number] = fact
	}
	for _, review := range prior.Reviews {
		previous[review.Number] = review
	}
	merge := func(old []PublicRound, new []RoundFacts) ([]RoundFacts, error) {
		out := make([]RoundFacts, 0, len(old)+len(new))
		positions := map[string]int{}
		for _, round := range old {
			positions[round.Key] = len(out)
			out = append(out, RoundFacts{Key: round.Key, Completed: true, Finalized: true, Complete: round.Complete, Found: round.Found, Accepted: round.Accepted, Routed: round.Routed, ObservedAt: round.ObservedAt, DurationMilliseconds: round.DurationMilliseconds})
		}
		seen := map[string]bool{}
		for _, round := range new {
			if !round.Completed || !round.Finalized {
				continue
			}
			if seen[round.Key] {
				return nil, unavailable()
			}
			seen[round.Key] = true
			if i, exists := positions[round.Key]; exists {
				out[i] = round
			} else {
				positions[round.Key] = len(out)
				out = append(out, round)
			}
		}
		return out, nil
	}
	out := make([]ReviewFacts, 0, len(m.Reviews))
	for _, enrolled := range m.Reviews {
		fact, found := current[enrolled.Number]
		old, retained := previous[enrolled.Number]
		if !found && !retained {
			continue
		}
		if !found {
			fact = ReviewFacts{Number: enrolled.Number, Lifecycle: old.Lifecycle}
		}
		if retained {
			fact.CLIHasMore = fact.CLIHasMore || old.CLIHasMore
			fact.HostedHasMore = fact.HostedHasMore || old.HostedHasMore
			fact.CLI, err = merge(old.CLI, fact.CLI)
			if err != nil {
				return nil, err
			}
			fact.Hosted, err = merge(old.Hosted, fact.Hosted)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, fact)
	}
	return out, nil
}

func (s *Service) Configuration(ctx context.Context, scope core.Scope) (Configuration, error) {
	out := Configuration{}
	err := s.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		r, found, err := readRecord(ctx, tx, scope.ProjectID)
		if err != nil || !found {
			return err
		}
		m, err := DecodeManifest([]byte(r.manifest))
		if err != nil {
			return unavailable()
		}
		out = Configuration{r.revision, r.enabled, &m, r.refreshError}
		return nil
	})
	return out, err
}

func (s *Service) Preview(ctx context.Context, scope core.Scope, input Approved) (Preview, error) {
	input, err := approved(input)
	if err != nil {
		return Preview{}, err
	}
	out := Preview{ExpectedRevision: input.ExpectedRevision, Enabled: input.Enabled, Manifest: input.Manifest, SHA256: s.previewHash(scope, input)}
	err = s.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		r, _, err := readRecord(ctx, tx, scope.ProjectID)
		if err != nil {
			return err
		}
		if r.revision != input.ExpectedRevision {
			return conflict(r.revision)
		}
		if r.salt == "" {
			r.salt = core.NewID("public_salt")
		}
		out.Content, _, err = build(ctx, tx, scope.ProjectID, input.Manifest, r.salt, s.reader, r.enabled && input.Enabled && r.slug == input.Manifest.Slug)
		return err
	})
	return out, err
}

func (s *Service) Apply(ctx context.Context, scope core.Scope, key string, input Approved, preview string) (json.RawMessage, error) {
	input, err := approved(input)
	if err != nil {
		return nil, err
	}
	if preview != s.previewHash(scope, input) {
		return nil, core.Fail("publication_preview_conflict", "Preview the exact approved publication input before applying it.", 409)
	}
	return s.store.Mutate(ctx, scope, core.Mutation{Operation: "publication.replace", Target: "publication", Key: key, Request: input}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		r, found, err := readRecord(ctx, tx, scope.ProjectID)
		if err != nil {
			return nil, err
		}
		if r.revision != input.ExpectedRevision {
			return nil, conflict(r.revision)
		}
		data, _ := json.Marshal(input.Manifest)
		if found && r.manifest == string(data) && r.enabled == input.Enabled {
			return Receipt{r.revision, "unchanged"}, nil
		}
		var owner string
		err = tx.QueryRowContext(ctx, `SELECT project_id FROM project_publications WHERE slug=? AND project_id<>?`, input.Manifest.Slug, scope.ProjectID).Scan(&owner)
		if err == nil {
			return nil, core.Fail("publication_slug_conflict", "The public slug is unavailable.", 409)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, unavailable()
		}
		if !found {
			r.salt = core.NewID("public_salt")
		}
		changedRoute := found && r.slug != input.Manifest.Slug
		var content string
		if input.Enabled {
			_, content, err = build(ctx, tx, scope.ProjectID, input.Manifest, r.salt, s.reader, r.enabled && !changedRoute)
			if err != nil {
				return nil, err
			}
		}
		if input.Enabled && (!r.enabled || changedRoute) {
			r.generation = core.NewID("public_generation")
			r.publicRevision = 0
			r.updated = ""
		}
		r.revision++
		r.slug = input.Manifest.Slug
		r.manifest = string(data)
		r.enabled = input.Enabled
		_, err = tx.ExecContext(ctx, `INSERT INTO project_publications(project_id,revision,slug,enabled,manifest_json,round_salt,generation,public_revision,public_updated_at,refresh_error) VALUES(?,?,?,?,?,?,?,?,?,'') ON CONFLICT(project_id) DO UPDATE SET revision=excluded.revision,slug=excluded.slug,enabled=excluded.enabled,manifest_json=excluded.manifest_json,generation=excluded.generation,public_revision=excluded.public_revision,public_updated_at=excluded.public_updated_at,refresh_error=''`, scope.ProjectID, r.revision, r.slug, r.enabled, r.manifest, r.salt, r.generation, r.publicRevision, r.updated)
		if err != nil {
			return nil, unavailable()
		}
		if r.enabled {
			if err = publishContent(ctx, tx, r, content); err != nil {
				return nil, err
			}
		}
		// Disabling or changing routes must also terminate the old delivery.
		if err = core.MarkPublicProjectionChanged(ctx, scope.ProjectID); err != nil {
			return nil, err
		}
		return Receipt{r.revision, "updated"}, nil
	})
}

func (s *Service) Unpublish(ctx context.Context, scope core.Scope, key string, revision int64) (json.RawMessage, error) {
	if revision < 0 || revision >= math.MaxInt64 {
		return nil, invalid()
	}
	return s.store.Mutate(ctx, scope, core.Mutation{Operation: "publication.unpublish", Target: "publication", Key: key, Request: struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}{revision}}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		r, found, err := readRecord(ctx, tx, scope.ProjectID)
		if err != nil {
			return nil, err
		}
		if r.revision != revision {
			return nil, conflict(r.revision)
		}
		if !found || !r.enabled {
			return Receipt{revision, "unchanged"}, nil
		}
		if _, err = tx.ExecContext(ctx, `UPDATE project_publications SET enabled=0,revision=revision+1 WHERE project_id=?`, scope.ProjectID); err != nil {
			return nil, unavailable()
		}
		if err = core.MarkPublicProjectionChanged(ctx, scope.ProjectID); err != nil {
			return nil, err
		}
		return Receipt{revision + 1, "updated"}, nil
	})
}

func refresh(ctx context.Context, tx *sql.Tx, r record, m Manifest, reader SourceReader) error {
	_, content, err := build(ctx, tx, r.project, m, r.salt, reader, true)
	if err != nil {
		return err
	}
	return publishContent(ctx, tx, r, content)
}

func publishContent(ctx context.Context, tx *sql.Tx, r record, content string) error {
	var previous string
	err := tx.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(content_json AS BLOB))<=262144 THEN content_json ELSE NULL END FROM public_project_snapshots WHERE project_id=? AND generation=? AND revision=?`, r.project, r.generation, r.publicRevision).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return unavailable()
	}
	if previous == content {
		_, err = tx.ExecContext(ctx, `UPDATE project_publications SET refresh_error='' WHERE project_id=?`, r.project)
		return err
	}
	if r.publicRevision >= math.MaxInt64 {
		return unavailable()
	}
	now := time.Now().UTC()
	at := now.Format(time.RFC3339Nano)
	// Publication time is monotonic within a generation even across clock rollback.
	if old, e := time.Parse(time.RFC3339Nano, r.updated); e == nil && old.After(now) {
		at = r.updated
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO public_project_snapshots(project_id,generation,revision,content_json,created_at) VALUES(?,?,?,?,?)`, r.project, r.generation, r.publicRevision+1, content, at); err != nil {
		return unavailable()
	}
	if _, err = tx.ExecContext(ctx, `UPDATE project_publications SET public_revision=?,public_updated_at=?,refresh_error='' WHERE project_id=?`, r.publicRevision+1, at, r.project); err != nil {
		return unavailable()
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM public_project_snapshots WHERE project_id=? AND (generation<>? OR (revision<>? AND (revision<=? OR julianday(created_at)<julianday(?))))`, r.project, r.generation, r.publicRevision+1, r.publicRevision+1-1000, now.Add(-7*24*time.Hour).Format(time.RFC3339Nano)); err != nil {
		return unavailable()
	}
	return nil
}

// Refresh is called inside the authoritative current-review transaction. A
// failed projection is contained and reported privately, preserving both the
// authoritative review write and the last successfully published snapshot.
func Refresh(ctx context.Context, tx *sql.Tx, project string, reader SourceReader) error {
	var version string
	err := tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:public_site'`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil || version != "1" {
		return unavailable()
	}
	r, found, err := readRecord(ctx, tx, project)
	if err != nil {
		// A malformed optional projection has made no writes yet. Contain that
		// failure just like a later builder failure, retaining authoritative work.
		if _, e := tx.ExecContext(ctx, `UPDATE project_publications SET refresh_error='projection_unavailable' WHERE project_id=?`, project); e != nil {
			return unavailable()
		}
		return nil
	}
	if !found || !r.enabled {
		return nil
	}
	if _, err = tx.ExecContext(ctx, `SAVEPOINT public_projection_refresh`); err != nil {
		return unavailable()
	}
	m, decodeErr := DecodeManifest([]byte(r.manifest))
	if decodeErr == nil {
		err = refresh(ctx, tx, r, m, reader)
	} else {
		err = unavailable()
	}
	if err != nil {
		if _, e := tx.ExecContext(ctx, `ROLLBACK TO public_projection_refresh`); e != nil {
			return unavailable()
		}
		if _, e := tx.ExecContext(ctx, `RELEASE public_projection_refresh`); e != nil {
			return unavailable()
		}
		_, e := tx.ExecContext(ctx, `UPDATE project_publications SET refresh_error='projection_unavailable' WHERE project_id=?`, project)
		if e != nil {
			return unavailable()
		}
		return nil
	}
	if _, err = tx.ExecContext(ctx, `RELEASE public_projection_refresh`); err != nil {
		return unavailable()
	}
	var revision int64
	if tx.QueryRowContext(ctx, `SELECT public_revision FROM project_publications WHERE project_id=?`, project).Scan(&revision) != nil {
		return unavailable()
	}
	if revision != r.publicRevision {
		return core.MarkPublicProjectionChanged(ctx, project)
	}
	return nil
}

func serving(ctx context.Context, tx *sql.Tx) error {
	var state string
	err := tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='restore:state'`).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) || err == nil && state == "coordination_active" {
		return nil
	}
	return notFound()
}

func publicTx(ctx context.Context, store *core.Store, fn func(context.Context, *sql.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := store.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return unavailable()
	}
	defer tx.Rollback()
	if err = serving(ctx, tx); err != nil {
		return err
	}
	return fn(ctx, tx)
}

func currentRecord(ctx context.Context, tx *sql.Tx, slug string) (record, error) {
	if !ValidSlug(slug) {
		return record{}, notFound()
	}
	r := record{slug: slug}
	err := tx.QueryRowContext(ctx, `SELECT project_id,generation,public_revision,public_updated_at FROM project_publications WHERE slug=? AND enabled=1`, slug).Scan(&r.project, &r.generation, &r.publicRevision, &r.updated)
	if errors.Is(err, sql.ErrNoRows) {
		return record{}, notFound()
	}
	if err != nil || r.publicRevision < 1 {
		return record{}, unavailable()
	}
	return r, nil
}

func snapshot(ctx context.Context, tx *sql.Tx, r record, revision int64) (Snapshot, error) {
	var raw, at string
	err := tx.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(content_json AS BLOB))<=262144 THEN content_json ELSE NULL END,created_at FROM public_project_snapshots WHERE project_id=? AND generation=? AND revision=? AND (revision=? OR julianday(created_at)>=julianday(?))`, r.project, r.generation, revision, r.publicRevision, time.Now().UTC().Add(-7*24*time.Hour).Format(time.RFC3339Nano)).Scan(&raw, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, notFound()
	}
	if err != nil {
		return Snapshot{}, unavailable()
	}
	content, err := decodeSnapshotContent(raw)
	if err != nil || content.Slug != r.slug {
		return Snapshot{}, unavailable()
	}
	return Snapshot{r.slug, r.generation, revision, at, cursor(r.generation, revision), content}, nil
}

func (s *Service) Current(ctx context.Context, slug string) (Snapshot, error) {
	var out Snapshot
	err := publicTx(ctx, s.store, func(ctx context.Context, tx *sql.Tx) error {
		r, err := currentRecord(ctx, tx, slug)
		if err != nil {
			return err
		}
		out, err = snapshot(ctx, tx, r, r.publicRevision)
		return err
	})
	return out, err
}

func cursor(generation string, revision int64) string {
	return generation + ":" + strconv.FormatInt(revision, 10)
}
func parseCursor(value string) (string, int64, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "public_generation_") || len(parts[0]) != 50 {
		return "", 0, resync()
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(parts[0], "public_generation_")); err != nil {
		return "", 0, resync()
	}
	revision, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || revision < 1 || strconv.FormatInt(revision, 10) != parts[1] {
		return "", 0, resync()
	}
	return parts[0], revision, nil
}
