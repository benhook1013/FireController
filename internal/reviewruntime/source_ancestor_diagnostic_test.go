package reviewruntime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestAncestorParentMovementDoesNotMisidentifySelectedComparison(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f := newCLIDriverFixture(t)
			child := f.reader.pulls[4]
			child.HeadSHA, child.BaseSHA, child.BaseRef = strings.Repeat("9", 40), f.headSHA, "parent-branch"
			f.reader.pulls[4] = child
			parent := f.reader.pulls[2]
			parent.HeadRef, parent.BaseRef, parent.BaseSHA = "parent-branch", "older-parent", strings.Repeat("c", 40)
			f.reader.pulls[2] = parent
			ancestor := f.reader.pulls[3]
			ancestor.HeadRef, ancestor.HeadSHA = "older-parent", strings.Repeat("d", 40)
			f.reader.pulls[3] = ancestor
			config := sourceConfiguration("stacked", 3, 2, 4)
			operation := sourceOperation(f, 4, channel)
			before := sourceState(t, f)
			plan, predecessors, err := resolveReviewSource(t.Context(), f.engine, newSourceReads(f.reader), 901, config, operation)
			sourceHoldReason(t, err, "predecessor_parent_moved")
			var hold *core.Error
			want := map[string]string{"reason": "predecessor_parent_moved", "pull_number": "2", "parent_pull_number": "3", "observed_base_sha": strings.Repeat("c", 40), "expected_parent_sha": strings.Repeat("d", 40)}
			if !errors.As(err, &hold) || !reflect.DeepEqual(hold.Details, want) {
				t.Fatal("original ancestor comparison was not identified", err)
			}
			if plan.Pull.Number != 4 || plan.ParentPullNumber != 2 || plan.Pull.BaseSHA != parent.HeadSHA || predecessors != nil {
				t.Fatal("ancestor refusal lost the verified target comparison")
			}
			operation.AllowOutOfOrder = true
			plan, predecessors, err = resolveReviewSource(t.Context(), f.engine, newSourceReads(f.reader), 901, config, operation)
			if err != nil || plan.Pull.Number != 4 || len(predecessors) != 0 {
				t.Fatal("explicit out-of-order selection still traversed ancestors", err)
			}
			child.BaseSHA = strings.Repeat("e", 40)
			f.reader.pulls[4] = child
			_, _, err = resolveReviewSource(t.Context(), f.engine, newSourceReads(f.reader), 901, config, operation)
			sourceHoldReason(t, err, "parent_moved")
			if sourceState(t, f) != before {
				t.Fatal("diagnostic or out-of-order source read changed durable review state")
			}
		})
	}
}

type refreshingAncestorReader struct {
	*cliDriverReader
	freshBase     string
	ancestorReads int
}

func (r *refreshingAncestorReader) Pull(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
	pull, err := r.cliDriverReader.Pull(ctx, repo, n)
	if n == 2 {
		r.ancestorReads++
		pull.BaseSHA = r.freshBase
	}
	return pull, err
}

func TestPreparingAncestorMismatchRefreshKeepsExactPredecessors(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f := newCLIDriverFixture(t)
			child := f.reader.pulls[4]
			child.HeadSHA, child.BaseSHA, child.BaseRef = strings.Repeat("9", 40), f.headSHA, "parent-branch"
			f.reader.pulls[4] = child
			parent := f.reader.pulls[2]
			parent.HeadRef, parent.BaseRef, parent.BaseSHA = "parent-branch", "older-parent", strings.Repeat("c", 40)
			f.reader.pulls[2] = parent
			ancestor := f.reader.pulls[3]
			ancestor.HeadRef, ancestor.HeadSHA = "older-parent", strings.Repeat("d", 40)
			f.reader.pulls[3] = ancestor
			cfg := sourceConfiguration("stacked", 3, 2, 4)
			op := sourceOperation(f, 4, channel)
			baseline := sourceState(t, f)
			raw := &refreshingAncestorReader{cliDriverReader: f.reader, freshBase: ancestor.HeadSHA}

			_, _, err := resolveReviewSource(t.Context(), f.engine, newSourceReads(raw), 901, cfg, op)
			sourceHoldReason(t, err, "predecessor_parent_moved")
			if raw.ancestorReads != 0 {
				t.Fatalf("dispatch-style source read did extra work: %d", raw.ancestorReads)
			}

			reads := newSourceReads(raw)
			reads.preparing = true
			plan, edges, err := resolveReviewSource(t.Context(), f.engine, reads, 901, cfg, op)
			if err != nil || plan.Pull.Number != 4 || len(edges) != 2 ||
				edges[0].Pull.Number != 2 || edges[0].Pull.BaseSHA != ancestor.HeadSHA ||
				edges[1].Pull.Number != 3 || raw.ancestorReads != 1 {
				t.Fatalf("did not reconcile the exact ancestor edge once: %+v %+v %v reads=%d", plan, edges, err, raw.ancestorReads)
			}
			if sourceState(t, f) != baseline {
				t.Fatal("refresh changed durable review state")
			}
		})
	}
}
