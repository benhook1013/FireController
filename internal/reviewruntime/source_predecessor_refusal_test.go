package reviewruntime

import (
	"errors"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestSourcePredecessorRefusalActualAncestorAndCause(t *testing.T) {
	f := newCLIDriverFixture(t)
	configureExplicitStack(t, f)
	operation := sourceOperation(f, 4, "cli")
	operation.ConfigurationRevision = 2
	before := sourceState(t, f)
	for _, tc := range []struct {
		cause string
		queue []int64
	}{{"not_in_queue", []int64{2, 4}}, {"discovery_incomplete", []int64{2, 3, 4}}} {
		_, _, err := resolveReviewSource(t.Context(), f.engine, newSourceReads(f.reader), 901, sourceConfiguration("stacked", tc.queue...), operation)
		var failure *core.Error
		if !errors.As(err, &failure) || failure.Code != "review_held" {
			t.Fatal(err)
		}
		want := map[string]string{"reason": "predecessor_unfinished", "pull_number": "3", "cause": tc.cause}
		if !reflect.DeepEqual(failure.Details, want) {
			t.Fatal("reported selected PR rather than actual ancestor", failure.Details, want)
		}
	}
	if sourceState(t, f) != before || f.fetches.Load() != 0 {
		t.Fatal("source refusal dispatched or changed local state")
	}
}
