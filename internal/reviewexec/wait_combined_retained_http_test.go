package reviewexec

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

// The caller reached this state through real CLI exhaustion, a retained retry
// baseline, a second preparation and ScheduleSourceRetry. No wait is fabricated.
func assertCombinedRetainedWaitHTTP(t *testing.T, f *attemptFixture, retained Operation) {
	t.Helper()
	if retained.Channel != "cli" || retained.Wait == nil || retained.Wait.SourceRetry == nil || retained.Wait.CLIAllowance == nil {
		t.Fatal("combined durable fixture required")
	}
	admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'combined-reader','Reader')`, attemptProject)
	tables := []string{"review_operations", "review_operation_waits", "review_execution_attempts", "review_source_retries", "receipts", "events"}
	snapshot := func() string {
		var state string
		for _, table := range tables {
			state += snapshotTable(t, f, table)
		}
		return state
	}
	before := snapshot()
	base := "/api/v1/projects/" + attemptProject + "/reviews/operations/" + retained.ID
	for _, version := range []string{"3", "4", "5", "6"} {
		for _, suffix := range []string{"", "/status"} {
			t.Run("retained-http/v"+version+suffix, func(t *testing.T) {
				response := versionWaitRead(t, f, base+suffix, version)
				var got OperationStatus
				if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				expected := *retained.Wait
				if version != "4" && version != "6" {
					expected.SourceRetry = nil
				}
				if version == "4" {
					expected.CLIAllowance = nil
				}
				if got.ID != retained.ID || got.State != "queued" || got.AttemptID != "" || !reflect.DeepEqual(got.Wait, &expected) {
					t.Fatal("durable negotiated facts changed", version, response.Body.String())
				}
			})
		}
	}
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		calls.Add(1)
		if r.Header.Get(operationWaitHeader) != "6" {
			t.Error("client did not request combined projection")
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	credential := filepath.Join(t.TempDir(), "reader.token")
	if err := os.WriteFile(credential, []byte(f.reader), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(t.Context(), profile.Binding{Context: profile.Context{Profile: "fixture", ServerURL: srv.URL, InstanceID: f.store.InstanceID(), ProjectID: attemptProject, WorkerID: "combined-reader"}, CredentialFile: credential})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	expectedJSON, _ := json.Marshal(retained.Wait)
	var expected map[string]any
	_ = json.Unmarshal(expectedJSON, &expected)
	for _, compact := range []bool{false, true} {
		name := "full"
		if compact {
			name = "compact"
		}
		t.Run("retained-client/"+name, func(t *testing.T) {
			var wait *client.ReviewOperationWait
			if compact {
				got, err := c.ReviewOperationStatus(t.Context(), retained.ID)
				if err != nil {
					t.Fatal("actual compact decode", err)
				}
				if got.ID != retained.ID || got.State != "queued" || got.AttemptID != "" {
					t.Fatal("identity changed")
				}
				wait = got.Wait
			} else {
				got, err := c.ReviewOperation(t.Context(), retained.ID)
				if err != nil {
					t.Fatal("actual full decode", err)
				}
				if got.ID != retained.ID || got.State != "queued" || got.AttemptID != "" {
					t.Fatal("identity changed")
				}
				wait = got.Wait
			}
			data, _ := json.Marshal(wait)
			var got map[string]any
			_ = json.Unmarshal(data, &got)
			if !reflect.DeepEqual(got, expected) {
				t.Fatal("strict client lost retained facts", string(data), string(expectedJSON))
			}
		})
	}
	if calls.Load() != 2 {
		t.Fatal("client retried", calls.Load())
	}
	if snapshot() != before {
		t.Fatal("read-only negotiation changed durable intent, receipts or events")
	}
}
