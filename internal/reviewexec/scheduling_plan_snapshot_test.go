package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/benhook1013/FireController/internal/core"
	"reflect"
	"testing"
)

func TestAutomaticPlanErrorReasonsAreLocalAndSanitized(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{core.Fail("overloaded", "private driver value", 503), "local_planning_overloaded"},
		{core.Fail("storage_unavailable", "private SQL", 500), "local_planning_storage_unavailable"},
		{core.Fail("forbidden", "private principal", 403), "local_planning_authorization_changed"},
		{context.DeadlineExceeded, "local_planning_deadline"}, {context.Canceled, "local_planning_cancelled"},
		{automaticProjectStateFailure("overloaded", "private invalid state", 503), "local_planning_state_invalid"},
		{errors.New("private unclassified detail"), "local_planning_failed"},
	}
	for _, c := range cases {
		if got := automaticPlanErrorReason(fmt.Errorf("wrapped: %w", c.err)); got != c.want {
			t.Fatalf("got %s want %s", got, c.want)
		}
	}
	// Source selection still uses the provider classifier, independently.
	if runReason(context.DeadlineExceeded) != "source_fetch_timeout" {
		t.Fatal("source classifier changed")
	}
}
func TestAutomaticPlanFrameMemoPreservesDependenciesAndBoundaries(t *testing.T) {
	f := frontFixture(t)
	for _, lane := range []string{"hosted", "cli"} {
		frontSelect(t, f, "memo-frame-"+lane, lane, &refreshTestReader{})
		err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			frame, found, err := readFrontFrame(ctx, tx, attemptProject, lane)
			if err != nil || !found {
				t.Fatalf("missing frame %v %v", found, err)
			}
			memo := context.WithValue(ctx, automaticFrameKey{}, automaticFrameSnapshot{attemptProject, lane, frame, found, nil})
			for _, pull := range []int64{1, 2, 4, 999} {
				want, err := automaticDependenciesChannel(ctx, tx, attemptProject, pull, lane)
				if err != nil {
					return err
				}
				got, err := automaticDependenciesChannel(memo, tx, attemptProject, pull, lane)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s PR%d dependencies changed", lane, pull)
				}
			}
			poison := errors.New("memo error")
			poisoned := context.WithValue(ctx, automaticFrameKey{}, automaticFrameSnapshot{attemptProject, lane, frontFrame{}, false, poison})
			if _, _, err := automaticDependencyFrame(poisoned, tx, attemptProject, lane); !errors.Is(err, poison) {
				t.Fatal("memo lost error")
			}
			mismatch := context.WithValue(ctx, automaticFrameKey{}, automaticFrameSnapshot{"other-project", lane, frontFrame{}, false, poison})
			if _, ok, err := automaticDependencyFrame(mismatch, tx, attemptProject, lane); err != nil || !ok {
				t.Fatal("cross-project memo reused", err)
			}
			mismatch = context.WithValue(ctx, automaticFrameKey{}, automaticFrameSnapshot{attemptProject, "other-channel", frontFrame{}, false, poison})
			if _, ok, err := automaticDependencyFrame(mismatch, tx, attemptProject, lane); err != nil || !ok {
				t.Fatal("cross-channel memo reused", err)
			}
			// Parent context is unchanged: memo never escapes this planning invocation.
			if ctx.Value(automaticFrameKey{}) != nil {
				t.Fatal("memo escaped parent context")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
