package client_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestJobRevisionTypedClientRejectsWrongHistoricalIdentity(t *testing.T) {
	for _, damage := range []string{"none", "wrong-project", "wrong-job", "wrong-revision", "wrong-state-revision", "unknown-field", "oversized"} {
		t.Run(damage, func(t *testing.T) {
			state := client.JobState{ID: "job-one", ProjectID: "project-one", Title: "Original", Brief: "Historical body", Status: "active", Revision: 7, CreatedAt: "2026-10-15T00:00:00Z", UpdatedAt: "2026-10-15T00:00:00Z"}
			entry := client.JobHistoryEntry{Revision: 7, State: state, ActorID: "actor-one", CreatedAt: state.UpdatedAt}
			switch damage {
			case "wrong-project":
				entry.State.ProjectID = "other"
			case "wrong-job":
				entry.State.ID = "other"
			case "wrong-revision":
				entry.Revision = 8
			case "wrong-state-revision":
				entry.State.Revision = 8
			}
			raw, _ := json.Marshal(entry)
			if damage == "unknown-field" {
				raw = []byte(strings.TrimSuffix(string(raw), "}") + `,"extra":"private"}`)
			}
			if damage == "oversized" {
				raw = append([]byte(strings.Repeat(" ", 4<<20)), raw...)
			}
			var reads atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				reads.Add(1)
				if r.Method != "GET" || r.URL.Path != "/api/v1/projects/project-one/jobs/job-one/history/7" || r.URL.RawQuery != "" || r.Header.Get("Idempotency-Key") != "" {
					t.Error("exact read identity changed")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(raw)
			}))
			defer s.Close()
			c := newClient(t, s)
			value, err := c.JobRevision(t.Context(), "job-one", 7)
			if damage == "none" {
				if err != nil || value.State.Brief != entry.State.Brief || value.State.Progress != nil {
					t.Fatal("legacy state not retained", err)
				}
			} else {
				var e *client.Error
				if !errors.As(err, &e) || e.Code != "invalid_response" {
					t.Fatal("damaged history accepted", err)
				}
			}
			if reads.Load() != 1 {
				t.Fatal("read expanded or retried")
			}
		})
	}
}
