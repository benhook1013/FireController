package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

const maxMessageStreamFrame = 1 << 20

// RegisterMessageStream registers only the selected-worker incoming feed.
// The shutdown signal cancels persistent streams without interrupting ordinary
// requests that are draining gracefully.
func RegisterMessageStream(mux *http.ServeMux, store *core.Store, shutdownContext context.Context) {
	registerEventStream(mux, store, shutdownContext, "GET /api/v1/projects/{project_id}/message-events/stream", false)
}

// RegisterProjectStream registers the worker-independent project metadata feed.
func RegisterProjectStream(mux *http.ServeMux, store *core.Store, shutdownContext context.Context) {
	registerEventStream(mux, store, shutdownContext, "GET /api/v1/projects/{project_id}/project-events/stream", true)
}

func registerEventStream(mux *http.ServeMux, store *core.Store, shutdownContext context.Context, pattern string, project bool) {
	a := &api{store: store}
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		streamContext, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(shutdownContext, cancel)
		if shutdownContext.Err() != nil {
			cancel()
		}
		defer func() {
			stop()
			cancel()
		}()
		a.eventStream(w, r.WithContext(streamContext), project)
	})
}

func (a *api) eventStream(w http.ResponseWriter, r *http.Request, project bool) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		message := "Use GET for the message event stream."
		if project {
			message = "Use GET for the project event stream."
		}
		core.WriteError(w, core.Fail("method_not_allowed", message, http.StatusMethodNotAllowed))
		return
	}
	authCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	scope, err := a.store.Scope(r.WithContext(authCtx), "read")
	cancel()
	if err != nil {
		core.WriteError(w, err)
		return
	}
	invalid := core.Fail("validation_failed", "Use one snapshot cursor and optional limit 1-100 for the selected worker's stream.", 400)
	if project {
		invalid = core.Fail("validation_failed", "Use one project snapshot cursor and optional limit 1-100.", 400)
	}
	if !project && scope.WorkerID == "" || len(r.URL.RawQuery) > 16<<10 {
		core.WriteError(w, invalid)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		core.WriteError(w, invalid)
		return
	}
	for name, values := range query {
		if len(values) != 1 || name != "cursor" && name != "limit" {
			core.WriteError(w, invalid)
			return
		}
	}
	if query.Get("cursor") == "" {
		core.WriteError(w, invalid)
		return
	}
	limit, err := core.PageLimit(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if project && !query.Has("limit") {
		limit = 25
	}
	controller := http.NewResponseController(w)
	// Clear fcd's ordinary whole-response WriteTimeout. Every actual stream
	// frame has its own short deadline; idle streams have no write in flight.
	if err = controller.SetWriteDeadline(time.Time{}); err != nil {
		core.WriteError(w, core.Fail("stream_unavailable", "This transport cannot provide bounded streaming writes.", 503))
		return
	}
	started, transportFailed := false, false
	write := func(frame []byte) error {
		if r.Context().Err() != nil {
			return r.Context().Err()
		}
		if len(frame) > maxMessageStreamFrame {
			return core.Fail("stream_frame_too_large", "The event batch exceeds the stream frame bound; reconnect with a smaller limit.", 503)
		}
		if err := controller.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
			transportFailed = true
			return err
		}
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Accel-Buffering", "no")
			w.Header().Set("X-Request-ID", core.NewID("request"))
			w.WriteHeader(http.StatusOK)
			started = true
		}
		n, err := w.Write(frame)
		if err == nil && n != len(frame) {
			err = io.ErrShortWrite
		}
		if err == nil {
			err = controller.Flush()
		}
		clearErr := controller.SetWriteDeadline(time.Time{})
		if err == nil {
			err = clearErr
		}
		if err != nil {
			transportFailed = true
		}
		return err
	}
	frame := func(kind, cursor string, data []byte) error {
		var b bytes.Buffer
		b.WriteString("event: " + kind + "\n")
		if cursor != "" {
			b.WriteString("id: " + cursor + "\n")
		}
		b.WriteString("data: ")
		b.Write(data)
		b.WriteString("\n\n")
		return write(b.Bytes())
	}
	heartbeat := func() error { return write([]byte(": keepalive\n\n")) }
	if project {
		err = a.store.StreamProjectEvents(r.Context(), scope, query.Get("cursor"), limit, func(page core.ProjectEventPage) error {
			data, err := json.Marshal(page)
			if err != nil {
				return core.Fail("stream_failed", "The event batch could not be encoded.", 500)
			}
			return frame("project-events", page.EventCursor, data)
		}, heartbeat)
	} else {
		err = a.store.StreamMessageEvents(r.Context(), scope, query.Get("cursor"), limit, func(page core.EventPage) error {
			data, err := json.Marshal(page)
			if err != nil {
				return core.Fail("stream_failed", "The event batch could not be encoded.", 500)
			}
			return frame("message-events", page.Cursor, data)
		}, heartbeat)
	}
	if err == nil || r.Context().Err() != nil || transportFailed {
		return
	}
	if !started {
		_ = controller.SetWriteDeadline(time.Now().Add(2 * time.Second))
		core.WriteError(w, err)
		return
	}
	failure := &core.Error{Code: "stream_failed", Message: "The message event stream could not continue."}
	if project {
		failure.Message = "The project event stream could not continue."
	}
	var known *core.Error
	if errors.As(err, &known) {
		failure = &core.Error{Code: known.Code, Message: known.Message}
	}
	data, _ := json.Marshal(struct {
		Error     *core.Error `json:"error"`
		RequestID string      `json:"request_id"`
	}{failure, w.Header().Get("X-Request-ID")})
	_ = frame("error", "", data)
}
