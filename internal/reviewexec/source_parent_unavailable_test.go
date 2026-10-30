package reviewexec

import (
	"context"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type unavailableParentReader struct{ failure error }

func (unavailableParentReader) Repository(context.Context, int64) (reviewprovider.Repository, error) {
	return reviewprovider.Repository{ID: 901, FullName: "owner/repo", DefaultBranch: "main"}, nil
}
func (unavailableParentReader) Pull(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
	return reviewprovider.Pull{RepositoryID: 901, Number: n, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "codex/former-parent", ChangedFiles: 2}, nil
}
func (r unavailableParentReader) ParentPull(context.Context, string, string) (reviewprovider.Pull, error) {
	return reviewprovider.Pull{}, r.failure
}

func TestParentUnavailableOriginalFactsSurviveSupervisorAndNegotiatedReads(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f := supervisorFixture(t)
			queued := f.enqueue(t, attemptProject, "missing-parent", channel, 2, 1)
			driver := &controlledDriver{prepare: func(ctx context.Context, o Operation, c Configuration, _ Attempt) (Preparation, error) {
				c.Mode = "stacked"
				_, err := ResolveSource(ctx, unavailableParentReader{}, 901, c, Request{ExpectedConfigurationRevision: o.ConfigurationRevision, PullNumber: o.PullNumber, Channel: o.Channel, HeadSHA: strings.Repeat("a", 40)})
				return Preparation{}, err
			}}
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
				t.Fatal(err)
			}
			scope := evidenceReadScope(t, f, attemptProject, f.reader)
			status, err := f.e.ReadOperationStatus(t.Context(), scope, queued.ID)
			want := &SourceRefusal{PullNumber: 2, BaseRef: "codex/former-parent"}
			if err != nil || status.State != "held" || status.Reason != "parent_unavailable" || !reflect.DeepEqual(status.SourceRefusal, want) {
				t.Fatal(status, err)
			}
			if driver.calls.Load() != 0 || driver.prepares.Load() != 1 {
				t.Fatal("diagnostic altered provider admission")
			}
			a := Attempt{ID: status.AttemptID, ProjectID: attemptProject, OperationID: queued.ID}
			before := attemptState(t, f, a)
			for _, compact := range []bool{false, true} {
				for _, header := range []string{"", "true", "v2", "v3", "unknown"} {
					r := httptest.NewRequest("GET", "/api/v1/projects/"+attemptProject+"/reviews/operations/"+queued.ID, nil)
					r.SetPathValue("project_id", attemptProject)
					r.SetPathValue("operation_id", queued.ID)
					r.Header.Set("Authorization", "Bearer "+f.reader)
					r.Header.Set("X-FC-Source-Refusal", header)
					w := httptest.NewRecorder()
					if compact {
						f.e.readOperationStatusHTTP(w, r)
					} else {
						f.e.readOperationHTTP(w, r)
					}
					var body struct {
						SourceRefusal *SourceRefusal `json:"source_refusal"`
					}
					if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
						t.Fatal(w.Code, w.Body.String())
					}
					if (header == "v2" || header == "v3") && !reflect.DeepEqual(body.SourceRefusal, want) || header != "v2" && header != "v3" && body.SourceRefusal != nil {
						t.Fatal("negotiation changed original facts or broke an older client", header, w.Body.String())
					}
				}
			}
			if before != attemptState(t, f, a) {
				t.Fatal("reads changed accounting")
			}
			if _, err := f.store.DB().Exec(`UPDATE review_execution_transitions SET source_refusal_json='null' WHERE attempt_id=?`, a.ID); err != nil {
				t.Fatal(err)
			}
			old, err := f.e.ReadOperationStatus(t.Context(), scope, queued.ID)
			if err != nil || old.SourceRefusal != nil {
				t.Fatal("old refusal facts fabricated", old, err)
			}
		})
	}
}

func TestParentUnavailableRefusalRejectsHybridsAndPrivateFields(t *testing.T) {
	base := map[string]string{"reason": "parent_unavailable", "pull_number": "2", "base_ref": "codex/former-parent", "private": "DO_NOT_RETAIN"}
	err := &core.Error{Code: "review_held", Details: base}
	got := sourceRefusalFromError(err, "held", "parent_unavailable", 2)
	if got == nil || got.PullNumber != 2 || got.BaseRef != base["base_ref"] || strings.Contains(sourceRefusalJSON(got), "DO_NOT_RETAIN") {
		t.Fatal(got)
	}
	for _, v := range []*SourceRefusal{
		{PullNumber: 2, BaseRef: "codex/former-parent", ParentPullNumber: 3},
		{PullNumber: 2, BaseRef: "codex/former-parent", ObservedBaseSHA: strings.Repeat("a", 40)},
		{PullNumber: 2, BaseRef: "bad\nbranch"}, {PullNumber: 0, BaseRef: "branch"}, {PullNumber: 2, BaseRef: strings.Repeat("a", 600)},
	} {
		if validSourceRefusal(v, "held", "parent_unavailable", 2) {
			t.Fatal("invalid missing-parent union accepted", v)
		}
	}
	if validSourceRefusal(got, "held", "predecessor_parent_moved", 9) || validSourceRefusal(got, "active", "parent_unavailable", 2) {
		t.Fatal("facts crossed refusal identity")
	}
}

func TestParentUnavailableReaderRefusalRetainsCheckedLookup(t *testing.T) {
	r := unavailableParentReader{failure: sourceHold("parent_unavailable")}
	_, err := ResolveSource(t.Context(), r, 901, Configuration{Mode: "stacked", Queue: []int64{2}, Channels: []string{"cli"}}, Request{PullNumber: 2, HeadSHA: strings.Repeat("a", 40), Channel: "cli"})
	got := sourceRefusalFromError(err, "held", "parent_unavailable", 2)
	if got == nil || got.PullNumber != 2 || got.BaseRef != "codex/former-parent" {
		t.Fatal("reader wrapper erased lookup identity", got, err)
	}
}

func TestSourceRefusalErrorCannotMixKnownVariantFields(t *testing.T) {
	for _, details := range []map[string]string{
		{"reason": "predecessor_parent_moved", "pull_number": "4", "parent_pull_number": "6", "observed_base_sha": strings.Repeat("a", 40), "expected_parent_sha": strings.Repeat("b", 40), "base_ref": "branch"},
		{"reason": "parent_unavailable", "pull_number": "2", "base_ref": "branch", "parent_pull_number": "6"},
	} {
		err := &core.Error{Code: "review_held", Details: details}
		if sourceRefusalFromError(err, "held", details["reason"], 2) != nil {
			t.Fatal("mixed refusal evidence normalized into authority", details)
		}
	}
}
