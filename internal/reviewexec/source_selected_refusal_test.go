package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type selectedMismatchReader struct{ calls int }

func (r *selectedMismatchReader) Repository(context.Context, int64) (reviewprovider.Repository, error) {
	r.calls++
	return reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, nil
}
func (r *selectedMismatchReader) Pull(context.Context, string, int64) (reviewprovider.Pull, error) {
	r.calls++
	return reviewprovider.Pull{RepositoryID: 801, Number: 2, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "parent-4", ChangedFiles: 1}, nil
}
func (r *selectedMismatchReader) ParentPull(context.Context, string, string) (reviewprovider.Pull, error) {
	r.calls++
	return reviewprovider.Pull{RepositoryID: 801, Number: 4, State: "open", HeadSHA: strings.Repeat("c", 40), BaseSHA: strings.Repeat("d", 40), BaseRef: "main", HeadRef: "parent-4", ChangedFiles: 1}, nil
}

func TestSelectedParentRefusalResolveSupervisorAndResponses(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f := supervisorFixture(t)
			previous := int64(1)
			if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "selected-mismatch-config", Configuration{ExpectedRevision: &previous, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{4, 2}, Channels: []string{channel}}); err != nil {
				t.Fatal(err)
			}
			receipt := f.enqueue(t, attemptProject, "selected-parent-refusal", channel, 2, 2)
			reader := &selectedMismatchReader{}
			driver := &controlledDriver{prepare: func(ctx context.Context, o Operation, c Configuration, a Attempt) (Preparation, error) {
				_, err := ResolveSource(ctx, reader, 801, c, Request{ExpectedConfigurationRevision: o.ConfigurationRevision, PullNumber: o.PullNumber, Channel: o.Channel, HeadSHA: o.HeadSHA})
				return Preparation{}, err
			}}
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
				t.Fatal(err)
			}
			want := &SourceRefusal{PullNumber: 2, ParentPullNumber: 4, ObservedBaseSHA: strings.Repeat("b", 40), ExpectedParentSHA: strings.Repeat("c", 40)}
			scope := evidenceReadScope(t, f, attemptProject, f.reader)
			status, err := f.e.ReadOperationStatus(t.Context(), scope, receipt.ID)
			if err != nil || status.State != "held" || status.Reason != "parent_moved" || !reflect.DeepEqual(status.SourceRefusal, want) || reader.calls != 3 || driver.calls.Load() != 0 {
				t.Fatal("actual source mismatch not retained before dispatch", status, reader.calls, err)
			}
			a := Attempt{ID: status.AttemptID, ProjectID: attemptProject, OperationID: receipt.ID}
			before := hostedObservationState(t, f, a)
			for _, view := range []string{"compact", "full", "collected"} {
				for _, header := range []string{"", "true", "v2", "v3", "v4"} {
					method, token, body := "GET", f.reader, ""
					if view == "collected" {
						method, token, body = "POST", f.writer, encoded(Request{ExpectedConfigurationRevision: 2, PullNumber: 2, Channel: channel, HeadSHA: strings.Repeat("a", 40)})
					}
					r := httptest.NewRequest(method, "/", strings.NewReader(body))
					r.SetPathValue("project_id", attemptProject)
					r.SetPathValue("operation_id", receipt.ID)
					r.Header.Set("Authorization", "Bearer "+token)
					r.Header.Set("X-FC-Source-Refusal", header)
					w := httptest.NewRecorder()
					switch view {
					case "compact":
						f.e.readOperationStatusHTTP(w, r)
					case "full":
						f.e.readOperationHTTP(w, r)
					default:
						r.Header.Set("Prefer", "wait=1")
						r.Header.Set("Idempotency-Key", "selected-parent-refusal")
						f.e.enqueueHTTP(w, r)
					}
					var got struct {
						SourceRefusal *SourceRefusal `json:"source_refusal"`
					}
					if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
						t.Fatal(view, header, w.Code, w.Body.String())
					}
					if header == "v4" && !reflect.DeepEqual(got.SourceRefusal, want) || header != "v4" && got.SourceRefusal != nil {
						t.Fatal("selected comparison escaped negotiated shape", view, header, w.Body.String())
					}
				}
			}
			if hostedObservationState(t, f, a) != before || reader.calls != 3 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 0 {
				t.Fatal("diagnostic read dispatched or changed native state")
			}
			for _, version := range []string{"1", "2", "3", "4"} {
				admissionSQL(t, f, `UPDATE metadata SET value=? WHERE key='schema:review_source_refusals'`, version)
				tx, err := f.store.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				validation := ValidateSourceRefusalsSnapshot(t.Context(), tx)
				tx.Rollback()
				_, readErr := f.e.ReadOperationStatus(t.Context(), scope, receipt.ID)
				if (validation == nil) != (version == "4") || (readErr == nil) != (version == "4") {
					t.Fatal("selected metadata accepted under old semantic format", version, validation, readErr)
				}
			}
			admissionSQL(t, f, `UPDATE review_execution_transitions SET source_refusal_json='null' WHERE attempt_id=?`, a.ID)
			legacy, err := f.e.ReadOperationStatus(t.Context(), scope, receipt.ID)
			if err != nil || legacy.SourceRefusal != nil {
				t.Fatal("old unknown selected hold fabricated metadata", legacy, err)
			}
		})
	}
}

func TestSelectedParentRefusalStrictBindingAndLegacyVariants(t *testing.T) {
	good := &SourceRefusal{PullNumber: 2, ParentPullNumber: 4, ObservedBaseSHA: strings.Repeat("a", 40), ExpectedParentSHA: strings.Repeat("b", 40)}
	if !validSourceRefusal(good, "held", "parent_moved", 2) || validSourceRefusal(good, "held", "predecessor_parent_moved", 2) {
		t.Fatal("selected and ancestor unions overlap")
	}
	for _, change := range []func(*SourceRefusal){func(v *SourceRefusal) { v.PullNumber = 6 }, func(v *SourceRefusal) { v.ParentPullNumber = 2 }, func(v *SourceRefusal) { v.ParentPullNumber = 0 }, func(v *SourceRefusal) { v.ExpectedParentSHA = v.ObservedBaseSHA }, func(v *SourceRefusal) { v.ObservedBaseSHA = "abc" }, func(v *SourceRefusal) { v.Cause = "discovery_incomplete" }, func(v *SourceRefusal) { v.BaseRef = "private-branch" }} {
		bad := *good
		change(&bad)
		if validSourceRefusal(&bad, "held", "parent_moved", 2) {
			t.Fatal("contradictory selected refusal accepted", bad)
		}
	}
	variants := []struct {
		reason string
		v      *SourceRefusal
		first  string
	}{{"predecessor_parent_moved", &SourceRefusal{PullNumber: 6, ParentPullNumber: 4, ObservedBaseSHA: good.ObservedBaseSHA, ExpectedParentSHA: good.ExpectedParentSHA}, "1"}, {"parent_unavailable", &SourceRefusal{PullNumber: 2, BaseRef: "parent"}, "2"}, {"predecessor_unfinished", &SourceRefusal{PullNumber: 4, Cause: "not_in_queue"}, "3"}, {"parent_moved", good, "4"}}
	for _, variant := range variants {
		for _, version := range []string{"1", "2", "3", "4"} {
			if sourceRefusalVersionAllows(variant.v, variant.reason, version) != (version >= variant.first) {
				t.Fatal("historical variant changed", variant.reason, version)
			}
		}
		for _, header := range []string{"true", "v2", "v3", "v4"} {
			expected := header == "v4" || variant.first == "1" || variant.first == "2" && (header == "v2" || header == "v3") || variant.first == "3" && header == "v3"
			if (sourceRefusalForResponse(variant.v, header, variant.reason) != nil) != expected {
				t.Fatal("legacy header changed existing variant", variant.reason, header)
			}
		}
	}
}

func TestRunSelectedParentRefusalHeaderAndOriginalChild(t *testing.T) {
	f := selectionFixture(t)
	receipt, _ := selectionEnqueue(t, f, "run-selected-mismatch", "cli", nil)
	reader := &refreshTestReader{repository: func(context.Context, int64) (reviewprovider.Repository, error) {
		return reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, nil
	}, pull: func(context.Context, string, int64) (reviewprovider.Pull, error) {
		p := lifecyclePull(2)
		p.ChangedFiles = 1
		return p, nil
	}}
	selectionNext(t, f, reader)
	run := selectionRead(t, f, receipt.ID)
	a := f.start(t, attemptProject, Receipt{ID: run.OperationID})
	want := &SourceRefusal{PullNumber: 2, ParentPullNumber: 4, ObservedBaseSHA: strings.Repeat("b", 40), ExpectedParentSHA: strings.Repeat("c", 40)}
	if _, err := f.e.advanceWithSourceRefusal(t.Context(), attemptProject, a.ID, a.Revision, "held", "parent_moved", "", want); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	before := selectionCounts(t, f)
	for _, method := range []string{"GET", "POST"} {
		for _, header := range []string{"", "true", "v2", "v3", "v4"} {
			path := "/api/v1/projects/" + attemptProject + "/reviews/selections/" + receipt.ID
			body := ""
			if method == "POST" {
				path = "/api/v1/projects/" + attemptProject + "/reviews/selections"
				body = `{"channel":"cli"}`
			}
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+f.writer)
			r.Header.Set("X-FC-Worker", "selection-worker")
			r.Header.Set("X-FC-Source-Refusal", header)
			r.Header.Set("Prefer", "wait=1")
			r.Header.Set("Idempotency-Key", "run-selected-mismatch")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			var state RunState
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &state) != nil || state.Operation == nil || state.Operation.ID != run.OperationID {
				t.Fatal(method, header, w.Code, w.Body.String())
			}
			if header == "v4" && !reflect.DeepEqual(state.Operation.SourceRefusal, want) || header != "v4" && state.Operation.SourceRefusal != nil {
				t.Fatal("run lost original selected refusal", method, header, state.Operation)
			}
		}
	}
	if selectionCounts(t, f) != before {
		t.Fatal("run diagnostics created or changed work")
	}
}

func TestSelectedParentRefusalFormatMigrationPreservesHistory(t *testing.T) {
	f := supervisorFixture(t)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "old-ancestor-refusal", "cli", 2, 1))
	old := &SourceRefusal{PullNumber: 4, ParentPullNumber: 6, ObservedBaseSHA: strings.Repeat("a", 40), ExpectedParentSHA: strings.Repeat("b", 40)}
	var err error
	a, err = f.e.advanceWithSourceRefusal(t.Context(), attemptProject, a.ID, a.Revision, "held", "predecessor_parent_moved", "", old)
	if err != nil {
		t.Fatal(err)
	}
	var schema string
	if err = f.store.DB().QueryRow(`SELECT sql FROM sqlite_schema WHERE name='review_execution_transitions'`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `UPDATE metadata SET value='3' WHERE key='schema:review_source_refusals'`)
	admissionSQL(t, f, `CREATE TRIGGER refuse_selected_format BEFORE UPDATE ON metadata WHEN NEW.key='schema:review_source_refusals' AND NEW.value='4' BEGIN SELECT RAISE(ABORT,'synthetic format failure'); END`)
	if err = EnsureSchema(t.Context(), f.store); err == nil {
		t.Fatal("failed format marker upgrade ignored")
	}
	var version, body string
	if err = f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_source_refusals'`).Scan(&version); err != nil || version != "3" {
		t.Fatal("failed upgrade changed old declared format", version, err)
	}
	admissionSQL(t, f, `DROP TRIGGER refuse_selected_format`)
	if err = EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if err = f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_source_refusals'`).Scan(&version); err != nil || version != "4" {
		t.Fatal(version, err)
	}
	if err = f.store.DB().QueryRow(`SELECT source_refusal_json FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, a.Revision).Scan(&body); err != nil || body != sourceRefusalJSON(old) {
		t.Fatal("format upgrade changed original history", body, err)
	}
	var after string
	if err = f.store.DB().QueryRow(`SELECT sql FROM sqlite_schema WHERE name='review_execution_transitions'`).Scan(&after); err != nil || after != schema {
		t.Fatal("semantic format marker changed table shape", err)
	}
}
