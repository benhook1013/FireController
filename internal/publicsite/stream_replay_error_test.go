package publicsite

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Keep a real HTTP writer so deadlines and flushes exercise the stream's actual
// transport. Inject the state change after a replay frame, before its next read.
type replayFaultWriter struct {
	http.ResponseWriter
	afterReplay func()
}

func (w *replayFaultWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *replayFaultWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if err == nil && bytes.HasPrefix(data, []byte("event: public-project\n")) && w.afterReplay != nil {
		fault := w.afterReplay
		w.afterReplay = nil
		fault()
	}
	return n, err
}

func TestPublicReplayLookupFailureSendsSafeErrorFrame(t *testing.T) {
	for _, mode := range []string{"unpublished", "generation_changed", "cursor_retired", "storage_failure", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			f := newPublicationFixture(t)
			manifest := testPublicManifest()
			if w := f.apply(t, Approved{0, true, manifest}, "publish-first"); w.Code != http.StatusOK {
				t.Fatal(w.Code)
			}
			first := f.public(t)
			manifest.Description = "Second approved public description"
			if w := f.apply(t, Approved{1, true, manifest}, "publish-second"); w.Code != http.StatusOK {
				t.Fatal(w.Code)
			}
			second := f.public(t)
			faultResult := make(chan error, 1)
			done := make(chan struct{})
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				ctx, cancel := context.WithCancel(r.Context())
				defer cancel()
				writer := &replayFaultWriter{ResponseWriter: w, afterReplay: func() {
					var err error
					switch mode {
					case "unpublished":
						_, err = f.store.DB().ExecContext(t.Context(), `UPDATE project_publications SET enabled=0 WHERE project_id=?`, f.project)
					case "generation_changed":
						_, err = f.store.DB().ExecContext(t.Context(), `UPDATE project_publications SET generation='replacement-generation' WHERE project_id=?`, f.project)
					case "cursor_retired":
						_, err = f.store.DB().ExecContext(t.Context(), `DELETE FROM public_project_snapshots WHERE project_id=? AND revision=?`, f.project, second.Revision)
					case "storage_failure":
						_, err = f.store.DB().ExecContext(t.Context(), `DROP TABLE public_project_snapshots`)
					case "canceled":
						cancel()
					}
					faultResult <- err
				}}
				f.mux.ServeHTTP(writer, r.WithContext(ctx))
			}))
			defer backend.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, backend.URL+"/api/v1/public/projects/showcase/events?cursor="+first.Cursor, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := backend.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK {
				t.Fatal("stream response", response.StatusCode, err)
			}
			<-done
			if err := <-faultResult; err != nil {
				t.Fatal("injected state change", err)
			}
			if !bytes.HasPrefix(body, []byte(": ready\n\nevent: public-project\nid: "+second.Cursor+"\n")) {
				t.Fatal("missing authoritative replay frame", string(body))
			}
			code := "resync_required"
			if mode == "unpublished" {
				code = "not_found"
			}
			if mode == "canceled" {
				if bytes.Contains(body, []byte("event: error")) {
					t.Fatal("canceled request emitted an error frame")
				}
			} else {
				want := "event: error\ndata: {\"error\":{\"code\":\"" + code + "\"}}\n\n"
				if !bytes.HasSuffix(body, []byte(want)) || bytes.Count(body, []byte("event: error\n")) != 1 {
					t.Errorf("want one exact safe error frame %q, got %q", want, body)
				}
			}
			for _, private := range []string{f.project, f.worker, f.store.InstanceID(), f.admin, f.reader, "SQL", "no such table"} {
				if strings.Contains(string(body), private) {
					t.Fatal("stream exposed private error context")
				}
			}
			if f.service.streams.Load() != 0 {
				t.Fatal("failed replay leaked stream accounting")
			}
		})
	}
}
