package reviewexec

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// Branch lookup uses explicitly published head refs; queue order is deliberately
// unavailable to the fake provider and cannot manufacture dependency identity.
type actualParentSelectionReader struct {
	mu    sync.Mutex
	pulls map[int64]reviewprovider.Pull
	calls []string
}

func (r *actualParentSelectionReader) Repository(_ context.Context, id int64) (reviewprovider.Repository, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, fmt.Sprintf("repository:%d", id))
	return reviewprovider.Repository{ID: id, FullName: "owner/repo", DefaultBranch: "main"}, nil
}

func (r *actualParentSelectionReader) Pull(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, fmt.Sprintf("pull:%d", number))
	pull, ok := r.pulls[number]
	if !ok {
		return reviewprovider.Pull{}, errors.New("unconfigured pull")
	}
	return pull, nil
}

func (r *actualParentSelectionReader) ParentPull(_ context.Context, _ string, branch string) (reviewprovider.Pull, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "parent:"+branch)
	var found reviewprovider.Pull
	for _, pull := range r.pulls {
		if pull.HeadRef != branch || pull.State != "open" || pull.Merged {
			continue
		}
		if found.Number != 0 {
			return reviewprovider.Pull{}, errors.New("ambiguous published branch")
		}
		found = pull
	}
	if found.Number == 0 {
		return found, errors.New("missing published branch")
	}
	return found, nil
}

func TestRunSelectionActualParentKeepsUnfinishedFindingLocal(t *testing.T) {
	for _, dependent := range []bool{false, true} {
		t.Run(map[bool]string{false: "independent-later-target", true: "actual-dependent-target"}[dependent], func(t *testing.T) {
			f := selectionFixture(t)
			original, evidence := roundCapture(t, f, 0, true)
			roundFinalize(t, f, original, evidence, 0, true)
			before := adjudicationRead(t, f, evidence)
			revision := int64(1)
			if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "mixed-published-dependencies", Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}}); err != nil {
				t.Fatal(err)
			}
			reader := &actualParentSelectionReader{pulls: map[int64]reviewprovider.Pull{
				2: {Number: 2, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), HeadRef: "agent-a", BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1},
				4: {Number: 4, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("c", 40), HeadRef: "agent-b", BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1},
			}}
			if dependent {
				pull := reader.pulls[4]
				pull.BaseRef, pull.BaseSHA = "agent-a", reader.pulls[2].HeadSHA
				reader.pulls[4] = pull
			}
			request, _ := selectionEnqueue(t, f, "mixed-selection", "cli", nil)
			selectionNext(t, f, reader)
			got := selectionRead(t, f, request.ID)
			// The initial bounded metadata window may look ahead to PR4;
			// reaching it later requires its own fresh comparison read.
			wantCalls := map[string]int{"repository:801": 1, "pull:2": 1, "pull:4": 2}
			if dependent {
				wantCalls["parent:agent-a"] = 1
				if got.State != "held" || got.OperationID != "" {
					t.Fatal("unfinished actual parent allowed dependent dispatch", got)
				}
				if count := attemptCount(t, f.store, `SELECT count(*) FROM review_operations`); count != 1 {
					t.Fatal("held dependency created another operation", count)
				}
			} else {
				if got.State != "selected" || got.OperationID == "" {
					t.Fatal("unrelated finding blocked independent work", got)
				}
				op, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), got.OperationID)
				if err != nil || op.PullNumber != 4 || op.HeadSHA != reader.pulls[4].HeadSHA {
					t.Fatal("selected wrong independent source", op, err)
				}
			}
			gotCalls := map[string]int{}
			for _, call := range reader.calls {
				gotCalls[call]++
			}
			if !reflect.DeepEqual(gotCalls, wantCalls) {
				t.Fatal("source reads exceeded exact selected comparisons", reader.calls, wantCalls)
			}
			if after := adjudicationRead(t, f, evidence); !reflect.DeepEqual(before, after) {
				t.Fatal("scheduling changed retained parent findings", before, after)
			}
		})
	}
}
