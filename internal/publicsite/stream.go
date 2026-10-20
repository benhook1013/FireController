package publicsite

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

type PublicEvent struct {
	Revision    int64  `json:"revision"`
	PublishedAt string `json:"published_at"`
	Cursor      string `json:"cursor"`
}

func (s *Service) eventAfter(ctx context.Context, slug, gen string, after int64) (PublicEvent, bool, error) {
	var out PublicEvent
	found := false
	err := publicTx(ctx, s.store, func(ctx context.Context, tx *sql.Tx) error {
		r, err := currentRecord(ctx, tx, slug)
		if err != nil {
			return err
		}
		if r.generation != gen || after > r.publicRevision {
			return resync()
		}
		var retained bool
		if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public_project_snapshots WHERE project_id=? AND generation=? AND revision=? AND (revision=? OR julianday(created_at)>=julianday(?)))`, r.project, gen, after, r.publicRevision, time.Now().UTC().Add(-7*24*time.Hour).Format(time.RFC3339Nano)).Scan(&retained) != nil {
			return unavailable()
		}
		if !retained {
			return resync()
		}
		err = tx.QueryRowContext(ctx, `SELECT revision,created_at FROM public_project_snapshots WHERE project_id=? AND generation=? AND revision>? AND (revision=? OR julianday(created_at)>=julianday(?)) ORDER BY revision LIMIT 1`, r.project, gen, after, r.publicRevision, time.Now().UTC().Add(-7*24*time.Hour).Format(time.RFC3339Nano)).Scan(&out.Revision, &out.PublishedAt)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return unavailable()
		}
		out.Cursor = cursor(gen, out.Revision)
		found = true
		return nil
	})
	return out, found, err
}

func (s *Service) eventsHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		core.WriteError(w, core.Fail("method_not_allowed", "Use GET for public events.", 405))
		return
	}
	if !publicQuery(w, r, "cursor") {
		return
	}
	gen, after, err := parseCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if s.streams.Add(1) > 256 {
		s.streams.Add(-1)
		core.WriteError(w, core.Fail("overloaded", "Public stream capacity is occupied; reconnect later.", 503))
		return
	}
	defer s.streams.Add(-1)
	var project string
	err = publicTx(r.Context(), s.store, func(ctx context.Context, tx *sql.Tx) error {
		record, err := currentRecord(ctx, tx, r.PathValue("slug"))
		project = record.project
		return err
	})
	if err != nil {
		core.WriteError(w, err)
		return
	}
	// Remote viewers share the loopback reverse proxy's address. Account each
	// attachment separately; service and store aggregate bounds still apply.
	client := core.NewID("public_stream")
	hints, release, err := s.store.SubscribePublicProjection(project, client)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	defer release()
	// Subscription precedes the authoritative replay read, so an intervening
	// commit is either in that snapshot or leaves a post-commit hint.
	event, found, err := s.eventAfter(r.Context(), r.PathValue("slug"), gen, after)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	controller := http.NewResponseController(w)
	if controller.SetWriteDeadline(time.Time{}) != nil {
		core.WriteError(w, unavailable())
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	write := func(data []byte) error {
		if err := controller.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
			return err
		}
		n, err := w.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		if err == nil {
			err = controller.Flush()
		}
		clear := controller.SetWriteDeadline(time.Time{})
		if err == nil {
			err = clear
		}
		return err
	}
	fail := func(err error) {
		if r.Context().Err() != nil {
			return
		}
		// No project, source or credential context is included in stream errors.
		code := "resync_required"
		if failure, ok := err.(*core.Error); ok && failure.Code == "not_found" {
			code = "not_found"
		}
		_ = write([]byte("event: error\ndata: {\"error\":{\"code\":" + strconv.Quote(code) + "}}\n\n"))
	}
	if write([]byte(": ready\n\n")) != nil {
		return
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		for found {
			// Every data frame comes from a newly publication-checked snapshot;
			// no private event or private snapshot metadata is forwarded.
			data, _ := json.Marshal(event)
			if write([]byte("event: public-project\nid: "+event.Cursor+"\ndata: "+string(data)+"\n\n")) != nil {
				return
			}
			after = event.Revision
			event, found, err = s.eventAfter(r.Context(), r.PathValue("slug"), gen, after)
			if err != nil {
				fail(err)
				return
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if write([]byte(": keepalive\n\n")) != nil {
				return
			}
		case <-hints:
			event, found, err = s.eventAfter(r.Context(), r.PathValue("slug"), gen, after)
			if err != nil {
				fail(err)
				return
			}
		}
	}
}
