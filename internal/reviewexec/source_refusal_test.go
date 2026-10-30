package reviewexec

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestSourceRefusalSupervisorRetainsOriginalComparisonAndReadCompatibility(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f := supervisorFixture(t)
			queued := f.enqueue(t, attemptProject, "ancestor-refusal", channel, 2, 1)
			want := &SourceRefusal{4, 6, strings.Repeat("c", 40), strings.Repeat("d", 40), "", ""}
			driver := &controlledDriver{prepare: func(context.Context, Operation, Configuration, Attempt) (Preparation, error) {
				return Preparation{}, &core.Error{Code: "review_held", Details: map[string]string{"reason": "predecessor_parent_moved", "pull_number": "4", "parent_pull_number": "6", "observed_base_sha": want.ObservedBaseSHA, "expected_parent_sha": want.ExpectedParentSHA, "private": "PRIVATE_NOT_RETAINED"}}
			}}
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
				t.Fatal(err)
			}
			if driver.calls.Load() != 0 || driver.prepares.Load() != 1 {
				t.Fatal("source refusal dispatched or retried provider work")
			}
			scope := evidenceReadScope(t, f, attemptProject, f.reader)
			status, err := f.e.ReadOperationStatus(t.Context(), scope, queued.ID)
			if err != nil || status.State != "held" || status.Reason != "predecessor_parent_moved" || !reflect.DeepEqual(status.SourceRefusal, want) {
				t.Fatal(status, err)
			}
			var a Attempt
			a.ID, a.ProjectID, a.OperationID = status.AttemptID, attemptProject, queued.ID
			before := attemptState(t, f, a)
			for _, expand := range []bool{false, true} {
				r := httptest.NewRequest("GET", "/api/v1/projects/"+attemptProject+"/reviews/operations/"+queued.ID, nil)
				r.SetPathValue("project_id", attemptProject)
				r.SetPathValue("operation_id", queued.ID)
				r.Header.Set("Authorization", "Bearer "+f.reader)
				r.Header.Set("X-FC-Operation-Evidence", "true")
				if expand {
					r.Header.Set("X-FC-Source-Refusal", "true")
				}
				w := httptest.NewRecorder()
				f.e.readOperationHTTP(w, r)
				var got Operation
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.State != status.State || got.Revision != status.Revision || got.AttemptID != status.AttemptID {
					t.Fatal(w.Code, w.Body.String())
				}
				if expand && !reflect.DeepEqual(got.SourceRefusal, want) || !expand && got.SourceRefusal != nil || strings.Contains(w.Body.String(), "PRIVATE_NOT_RETAINED") {
					t.Fatal("source details missing or leaked to an older client", got)
				}
			}
			if before != attemptState(t, f, a) {
				t.Fatal("diagnostic reads changed the journal or accounting")
			}
			if _, err := f.e.ReadOperationStatus(t.Context(), core.Scope{}, queued.ID); err == nil {
				t.Fatal("source details bypassed read authorization")
			}
			// An older retained hold has no original comparison metadata. Never
			// infer it from current provider facts, configuration or another hold.
			if _, err := f.store.DB().Exec(`UPDATE review_execution_transitions SET source_refusal_json='null' WHERE attempt_id=?`, a.ID); err != nil {
				t.Fatal(err)
			}
			legacy, err := f.e.ReadOperationStatus(t.Context(), scope, queued.ID)
			if err != nil || legacy.SourceRefusal != nil || legacy.State != "held" || legacy.Reason != status.Reason {
				t.Fatal("missing original facts were fabricated", legacy, err)
			}
			if _, err := f.store.DB().Exec(`DELETE FROM metadata WHERE key='schema:review_source_refusals'`); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.DB().Exec(`ALTER TABLE review_execution_transitions DROP COLUMN source_refusal_json`); err != nil {
				t.Fatal(err)
			}
			legacy, err = f.e.ReadOperationStatus(t.Context(), scope, queued.ID)
			if err != nil || legacy.SourceRefusal != nil || legacy.State != "held" {
				t.Fatal("pre-feature snapshot read failed", legacy, err)
			}
		})
	}
}

func TestSourceRefusalMetadataFailureRollsBackTheHeldTransition(t *testing.T) {
	f := supervisorFixture(t)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "refusal-rollback", "cli", 2, 1))
	if _, err := f.store.DB().Exec(`CREATE TRIGGER reject_source_refusal BEFORE INSERT ON review_execution_transitions WHEN NEW.source_refusal_json<>'null' BEGIN SELECT RAISE(ABORT,'controlled diagnostic storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	before := attemptState(t, f, a)
	want := &SourceRefusal{4, 6, strings.Repeat("a", 40), strings.Repeat("b", 40), "", ""}
	if _, err := f.e.advanceWithSourceRefusal(t.Context(), attemptProject, a.ID, a.Revision, "held", "predecessor_parent_moved", "", want); err == nil {
		t.Fatal("storage failure was ignored")
	}
	if attemptState(t, f, a) != before {
		t.Fatal("partial hold or reservation release survived a failed diagnostic write")
	}
}
