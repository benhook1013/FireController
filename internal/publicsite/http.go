package publicsite

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func (s *Service) Register(mux *http.ServeMux, shutdown context.Context) {
	if shutdown == nil {
		shutdown = context.Background()
	}
	base := "/api/v1/projects/{project_id}/publication"
	mux.HandleFunc("GET "+base, s.configurationHTTP)
	mux.HandleFunc("POST "+base+"/preview", s.previewHTTP)
	mux.HandleFunc("PUT "+base, s.applyHTTP)
	mux.HandleFunc("POST "+base+"/unpublish", s.unpublishHTTP)
	public := "/api/v1/public/projects/{slug}"
	mux.HandleFunc("GET "+public, s.currentHTTP)
	mux.HandleFunc("GET "+public+"/history", s.historyHTTP)
	mux.HandleFunc("GET "+public+"/history/{revision}", s.revisionHTTP)
	mux.HandleFunc("GET "+public+"/events", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(shutdown, cancel)
		defer func() { stop(); cancel() }()
		if shutdown.Err() != nil {
			cancel()
		}
		s.eventsHTTP(w, r.WithContext(ctx))
	})
}

func (s *Service) admin(w http.ResponseWriter, r *http.Request, write bool) (core.Scope, bool) {
	scope, err := s.store.AdminScope(r)
	if err == nil {
		_, err = core.QueryKeys(r, "validation_failed")
	}
	if err == nil && (scope.ProjectID == "" || write && scope.WorkerID == "") {
		err = core.Fail("worker_required", "Bind an explicit project and worker before changing publication.", 400)
	}
	if err != nil {
		core.WriteError(w, err)
		return core.Scope{}, false
	}
	return scope, true
}

func respond(w http.ResponseWriter, value any, err error) {
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, 200, value)
}

func (s *Service) configurationHTTP(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.admin(w, r, false)
	if !ok {
		return
	}
	out, err := s.Configuration(r.Context(), scope)
	respond(w, out, err)
}

func readInput(w http.ResponseWriter, r *http.Request, apply bool) (Approved, string, error) {
	var revision *int64
	var enabled *bool
	var manifest any
	preview := ""
	if apply {
		var body struct {
			ExpectedRevision *int64  `json:"expected_revision"`
			Enabled          *bool   `json:"enabled"`
			Manifest         any     `json:"manifest"`
			PreviewSHA256    *string `json:"preview_sha256"`
		}
		if err := core.ReadJSON(w, r, &body); err != nil {
			return Approved{}, "", err
		}
		if body.PreviewSHA256 == nil || len(*body.PreviewSHA256) != 64 {
			return Approved{}, "", invalid()
		}
		revision, enabled, manifest, preview = body.ExpectedRevision, body.Enabled, body.Manifest, *body.PreviewSHA256
	} else {
		var body struct {
			ExpectedRevision *int64 `json:"expected_revision"`
			Enabled          *bool  `json:"enabled"`
			Manifest         any    `json:"manifest"`
		}
		if err := core.ReadJSON(w, r, &body); err != nil {
			return Approved{}, "", err
		}
		revision, enabled, manifest = body.ExpectedRevision, body.Enabled, body.Manifest
	}
	if revision == nil || enabled == nil || manifest == nil {
		return Approved{}, "", invalid()
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return Approved{}, "", invalid()
	}
	m, err := DecodeManifest(raw)
	if err != nil {
		return Approved{}, "", invalid()
	}
	input, err := approved(Approved{*revision, *enabled, m})
	if err != nil {
		return Approved{}, "", err
	}
	return input, preview, nil
}

func (s *Service) previewHTTP(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.admin(w, r, true)
	if !ok {
		return
	}
	input, _, err := readInput(w, r, false)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	out, err := s.Preview(r.Context(), scope, input)
	respond(w, out, err)
}
func (s *Service) applyHTTP(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.admin(w, r, true)
	if !ok {
		return
	}
	input, preview, err := readInput(w, r, true)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	out, err := s.Apply(r.Context(), scope, key, input, preview)
	respond(w, out, err)
}
func (s *Service) unpublishHTTP(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.admin(w, r, true)
	if !ok {
		return
	}
	var body struct {
		ExpectedRevision *int64 `json:"expected_revision"`
	}
	if err := core.ReadJSON(w, r, &body); err != nil {
		core.WriteError(w, err)
		return
	}
	if body.ExpectedRevision == nil {
		core.WriteError(w, invalid())
		return
	}
	key, err := core.IdempotencyKey(r, "idempotency_key_required")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	out, err := s.Unpublish(r.Context(), scope, key, *body.ExpectedRevision)
	respond(w, out, err)
}

func publicQuery(w http.ResponseWriter, r *http.Request, keys ...string) bool {
	w.Header().Set("Cache-Control", "no-store")
	if len(r.URL.RawQuery) > 4096 {
		core.WriteError(w, invalid())
		return false
	}
	if _, err := core.QueryKeys(r, "validation_failed", keys...); err != nil {
		core.WriteError(w, err)
		return false
	}
	return true
}
func (s *Service) currentHTTP(w http.ResponseWriter, r *http.Request) {
	if !publicQuery(w, r) {
		return
	}
	out, err := s.Current(r.Context(), r.PathValue("slug"))
	respond(w, out, err)
}
func (s *Service) revisionHTTP(w http.ResponseWriter, r *http.Request) {
	if !publicQuery(w, r, "generation") {
		return
	}
	revision, err := strconv.ParseInt(r.PathValue("revision"), 10, 64)
	if err != nil || revision < 1 || strconv.FormatInt(revision, 10) != r.PathValue("revision") {
		core.WriteError(w, notFound())
		return
	}
	var out Snapshot
	err = publicTx(r.Context(), s.store, func(ctx context.Context, tx *sql.Tx) error {
		record, err := currentRecord(ctx, tx, r.PathValue("slug"))
		if err != nil {
			return err
		}
		if r.URL.Query().Get("generation") != record.generation {
			return resync()
		}
		out, err = snapshot(ctx, tx, record, revision)
		return err
	})
	respond(w, out, err)
}

type HistoryItem struct {
	Revision    int64  `json:"revision"`
	PublishedAt string `json:"published_at"`
}
type History struct {
	Slug       string        `json:"slug"`
	Generation string        `json:"generation"`
	Items      []HistoryItem `json:"items"`
	Cursor     string        `json:"cursor,omitempty"`
	HasMore    bool          `json:"has_more"`
}

func (s *Service) historyHTTP(w http.ResponseWriter, r *http.Request) {
	if !publicQuery(w, r, "limit", "cursor") {
		return
	}
	query := r.URL.Query()
	limit := int64(20)
	if query.Has("limit") {
		n, err := strconv.ParseInt(query.Get("limit"), 10, 64)
		if err != nil || n < 1 || n > 100 || strconv.FormatInt(n, 10) != query.Get("limit") {
			core.WriteError(w, invalid())
			return
		}
		limit = n
	}
	out := History{Slug: r.PathValue("slug"), Items: []HistoryItem{}}
	err := publicTx(r.Context(), s.store, func(ctx context.Context, tx *sql.Tx) error {
		record, err := currentRecord(ctx, tx, r.PathValue("slug"))
		if err != nil {
			return err
		}
		out.Generation = record.generation
		before := int64(math.MaxInt64)
		if query.Has("cursor") {
			gen, rev, err := parseCursor(query.Get("cursor"))
			if err != nil || gen != record.generation || rev > record.publicRevision {
				return resync()
			}
			before = rev
			var retained bool
			if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public_project_snapshots WHERE project_id=? AND generation=? AND revision=? AND (revision=? OR julianday(created_at)>=julianday(?)))`, record.project, gen, rev, record.publicRevision, time.Now().UTC().Add(-7*24*time.Hour).Format(time.RFC3339Nano)).Scan(&retained) != nil {
				return unavailable()
			}
			if !retained {
				return resync()
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT revision,created_at FROM public_project_snapshots WHERE project_id=? AND generation=? AND revision<? AND (revision=? OR julianday(created_at)>=julianday(?)) ORDER BY revision DESC LIMIT ?`, record.project, record.generation, before, record.publicRevision, time.Now().UTC().Add(-7*24*time.Hour).Format(time.RFC3339Nano), limit+1)
		if err != nil {
			return unavailable()
		}
		defer rows.Close()
		for rows.Next() {
			var item HistoryItem
			if rows.Scan(&item.Revision, &item.PublishedAt) != nil {
				return unavailable()
			}
			out.Items = append(out.Items, item)
		}
		if rows.Err() != nil {
			return unavailable()
		}
		out.HasMore = int64(len(out.Items)) > limit
		if out.HasMore {
			out.Items = out.Items[:limit]
			out.Cursor = cursor(record.generation, out.Items[len(out.Items)-1].Revision)
		}
		return nil
	})
	respond(w, out, err)
}
