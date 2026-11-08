package reviewexec

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This is the baseline restriction shipped by bf609's actual typed reader.
// Response compatibility must not depend on weakening that older validator.
func legacyOperationWaitAccepts(wait *OperationWait, channel, state, attempt string) bool {
	if wait == nil {
		return true
	}
	if state != "queued" || attempt != "" {
		return false
	}
	if wait.BaselineAttemptID == "" {
		return wait.RetryNotBefore == ""
	}
	return channel == "hosted" && hostedObservationID(wait.BaselineAttemptID, "review_attempt_") && validLegacyWaitTime(wait.RetryNotBefore)
}
func validLegacyWaitTime(value string) bool {
	at, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && !at.IsZero()
}

func versionWaitRead(t *testing.T, f *attemptFixture, path, version string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest("GET", path, nil)
	request.Header.Set("Authorization", "Bearer "+f.writer)
	request.Header.Set(operationWaitHeader, version)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal("versioned wait read", path, version, response.Code, response.Body.String())
	}
	return response
}

func TestCLIWaitResponseVersionsPreserveQueuedIdentityAndPause(t *testing.T) {
	f, a, _, _ := cliWaitFixture(t)
	if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	original := cliWaitRead(t, f, a.OperationID)
	base := "/api/v1/projects/" + attemptProject + "/reviews/operations/" + a.OperationID
	for _, paused := range []bool{false, true} {
		if paused {
			_, err := f.e.ControlOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), "pause-version-proof", a.OperationID, OperationControl{ExpectedRevision: original.Revision, Action: "pause"})
			if err != nil {
				t.Fatal(err)
			}
			original = cliWaitRead(t, f, a.OperationID)
		}
		before := snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_operation_waits") + snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "receipts")
		for _, version := range []string{"", "1", "2", "3", "unknown"} {
			for _, path := range []string{base, base + "/status", roundProgressPath(attemptProject) + "?pull_numbers=2&view=stages"} {
				response := versionWaitRead(t, f, path, version)
				var wait *OperationWait
				if strings.Contains(path, "/rounds/progress") {
					var progress RoundProgress
					if json.Unmarshal(response.Body.Bytes(), &progress) != nil {
						t.Fatal(response.Body.String())
					}
					for _, channel := range progress.Items[0].Channels {
						if channel.Channel == "cli" {
							wait = channel.Wait
						}
					}
				} else {
					var operation OperationStatus
					if json.Unmarshal(response.Body.Bytes(), &operation) != nil || operation.ID != original.ID || operation.State != "queued" || operation.AttemptID != "" || operation.Revision != original.Revision {
						t.Fatal("response changed operation", response.Body.String())
					}
					if strings.HasSuffix(path, "/status") && (operation.Stage != "queued" || operation.NextAction != "wait") {
						t.Fatal("response changed stage/action", operation)
					}
					wait = operation.Wait
					if version != "2" && version != "3" && !paused && response.Header().Get(queuedDiagnosticHeader) != "awaiting_scheduler" {
						t.Fatal("older reader received unsupported quota cause", response.Header())
					}
				}
				switch version {
				case "2", "3":
					expected := *original.Wait
					if version == "2" {
						expected.CLIAllowance = nil
					}
					if !reflect.DeepEqual(wait, &expected) {
						t.Fatal("new reader lost baseline/deadline/pause", path, wait, original.Wait)
					}
				case "1":
					if wait == nil || wait.Paused != paused || wait.BaselineAttemptID != "" || wait.RetryNotBefore != "" || !legacyOperationWaitAccepts(wait, "cli", "queued", "") {
						t.Fatal("legacy reader rejected projected wait", path, wait)
					}
				default:
					if wait != nil {
						t.Fatal("unnegotiated response exposed wait", wait)
					}
				}
			}
		}
		if !legacyOperationWaitAccepts(operationWaitForResponse(original.Wait, "cli", "1"), "cli", "queued", "") || legacyOperationWaitAccepts(original.Wait, "cli", "queued", "") {
			t.Fatal("legacy restriction fixture no longer proves regression")
		}
		if after := snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_operation_waits") + snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "receipts"); after != before {
			t.Fatal("read negotiation changed durable intent")
		}
	}
}

func TestHostedWaitResponseVersionsKeepOriginalMetadata(t *testing.T) {
	f, a, _, _ := queuedCooldown(t)
	original := cliWaitRead(t, f, a.OperationID)
	base := "/api/v1/projects/" + attemptProject + "/reviews/operations/" + a.OperationID
	for _, version := range []string{"1", "2", "3"} {
		for _, path := range []string{base, base + "/status", roundProgressPath(attemptProject) + "?pull_numbers=2&view=stages"} {
			response := versionWaitRead(t, f, path, version)
			var wait *OperationWait
			if strings.Contains(path, "/rounds/progress") {
				var p RoundProgress
				_ = json.Unmarshal(response.Body.Bytes(), &p)
				wait = p.Items[0].Channels[0].Wait
			} else {
				var p Operation
				_ = json.Unmarshal(response.Body.Bytes(), &p)
				wait = p.Wait
			}
			if !reflect.DeepEqual(wait, original.Wait) || !legacyOperationWaitAccepts(wait, "hosted", "queued", "") {
				t.Fatal("Hosted metadata changed", path, version, wait)
			}
		}
	}
}

func TestCLIWaitSelectionResponseNegotiatesSameChild(t *testing.T) {
	f := selectionFixture(t)
	receipt, _ := selectionEnqueue(t, f, "versioned-selection", "cli", nil)
	selectionNext(t, f, &refreshTestReader{})
	selection := selectionRead(t, f, receipt.ID)
	a := f.start(t, attemptProject, Receipt{ID: selection.OperationID})
	in := CLIAllowanceObservation{Anchor: attemptAnchor(attemptProject, 2), ObservedAt: stamp(), Remaining: 0, Limit: 3, RollingWindowMS: 3600000, ChangedFiles: 2}
	if err := f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, a.Revision, in); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	original := cliWaitRead(t, f, a.OperationID)
	for _, version := range []string{"1", "2", "3"} {
		response := versionWaitRead(t, f, "/api/v1/projects/"+attemptProject+"/reviews/selections/"+receipt.ID, version)
		var state RunState
		if json.Unmarshal(response.Body.Bytes(), &state) != nil || state.Selection.ID != receipt.ID || state.Operation == nil || state.Operation.ID != original.ID || state.Operation.State != "queued" {
			t.Fatal("selection child changed", response.Body.String())
		}
		expected := *original.Wait
		if version == "2" {
			expected.CLIAllowance = nil
		}
		if (version == "2" || version == "3") && !reflect.DeepEqual(state.Operation.Wait, &expected) || version == "1" && !legacyOperationWaitAccepts(state.Operation.Wait, "cli", "queued", "") {
			t.Fatal("selection ignored negotiation", version, state.Operation.Wait)
		}
	}
}

// Optional installed-build proof exercises the actual older typed reader rather
// than only the pinned schema predicate above; it never touches a live service.
func TestCLIWaitActualPriorClientReadsCurrentServer(t *testing.T) {
	executable := os.Getenv("FC_TEST_LEGACY_WAIT_CLIENT")
	if executable == "" {
		t.Skip("selected older package is an operator integration proof")
	}
	f, a, _, _ := cliWaitFixture(t)
	if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,'version-reader','Reader')`, attemptProject); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		if r.Header.Get(operationWaitHeader) != "1" {
			t.Error("selected older binary did not advertise original wait contract")
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	directory := t.TempDir()
	token := filepath.Join(directory, "fixture.token")
	profile := filepath.Join(directory, "profiles.json")
	if err := os.WriteFile(token, []byte(f.writer), 0600); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"version": 1, "profiles": map[string]any{"fixture": map[string]any{"server_url": server.URL, "instance_id": f.store.InstanceID(), "project_id": attemptProject, "worker_id": "version-reader", "credential_file": token}}})
	if err := os.WriteFile(profile, body, 0600); err != nil {
		t.Fatal(err)
	}
	for _, diagnostics := range []bool{false, true} {
		args := []string{"--profiles-file", profile, "--profile", "fixture", "reviews", "operation", a.OperationID}
		if diagnostics {
			args = append(args, "--diagnostics")
		}
		command := exec.CommandContext(t.Context(), executable, args...)
		var stdout, stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		if err := command.Run(); err != nil {
			t.Fatal("actual prior client rejected current queued wait", err, stderr.String())
		}
		var out OperationStatus
		if json.Unmarshal(stdout.Bytes(), &out) != nil || out.ID != a.OperationID || out.State != "queued" || out.AttemptID != "" || out.Wait == nil || out.Wait.BaselineAttemptID != "" {
			t.Fatal("actual prior client lost truthful queued state", stdout.String())
		}
	}
}

func TestCLIWaitRequestReplayPreservesReceiptAcrossVersions(t *testing.T) {
	f, a, _, _ := cliWaitFixture(t)
	if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	before := snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_operation_waits") + snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "receipts")
	body := `{"expected_configuration_revision":1,"pull_number":2,"channel":"cli","head_sha":"` + strings.Repeat("a", 40) + `"}`
	var original []byte
	for _, version := range []string{"1", "2", "3"} {
		request := httptest.NewRequest("POST", "/api/v1/projects/"+attemptProject+"/reviews/requests", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+f.writer)
		request.Header.Set("Idempotency-Key", "cli-wait-request")
		request.Header.Set("Prefer", "wait=0")
		request.Header.Set(operationWaitHeader, version)
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, request)
		var receipt Receipt
		if response.Code != 202 || json.Unmarshal(response.Body.Bytes(), &receipt) != nil || receipt.ID != a.OperationID || receipt.State != "queued" {
			t.Fatal("accepted request replay changed intent", version, response.Code, response.Body.String())
		}
		if original == nil {
			original = append([]byte{}, response.Body.Bytes()...)
		} else if !bytes.Equal(original, response.Body.Bytes()) {
			t.Fatal("metadata version changed accepted receipt")
		}
	}
	if after := snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_operation_waits") + snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "receipts"); after != before {
		t.Fatal("request replay recreated a queued intent")
	}
}

func TestCLIQueueOrderLegacyHintDoesNotCallItHosted(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	f.enqueue(t, attemptProject, "earlier-cli", "cli", 2, 1)
	later := f.enqueue(t, attemptProject, "later-cli", "cli", 4, 1)
	for _, version := range []string{"1", "2", "3"} {
		response := versionWaitRead(t, f, "/api/v1/projects/"+attemptProject+"/reviews/operations/"+later.ID+"/status", version)
		want := "queue_order"
		if version == "1" {
			want = "awaiting_scheduler"
		}
		if response.Header().Get(queuedDiagnosticHeader) != want {
			t.Fatal("legacy CLI hint would name Hosted priority", version, response.Header())
		}
	}
}
