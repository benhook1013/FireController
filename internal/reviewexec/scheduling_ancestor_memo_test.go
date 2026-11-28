package reviewexec

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestAutomaticDependenciesReuseSharedAncestorWithinSnapshot(t *testing.T) {
	f := schedulingFixture(t)
	frame := frontFrame{repository: 1, prefix: []frontPull{{Number: 2, HeadSHA: "parent"}, {Number: 4, HeadSHA: "intermediate", ParentPullNumber: 2}}}
	for pull := int64(10); pull < 60; pull++ {
		parent := int64(2)
		if pull%2 == 0 {
			parent = 4
		}
		frame.prefix = append(frame.prefix, frontPull{Number: pull, ParentPullNumber: parent})
	}
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		memo := newAutomaticDependencyRead(ctx, tx, attemptProject, "hosted", frame, true)
		for pull := int64(10); pull < 60; pull++ {
			direct, err := newAutomaticDependencyRead(ctx, tx, attemptProject, "hosted", frame, true).forPull(pull)
			if err != nil {
				return err
			}
			cached, err := memo.forPull(pull)
			if err != nil {
				return err
			}
			if encoded(direct) != encoded(cached) {
				return fmt.Errorf("pull %d: planning facts changed: direct %v, memo %v", pull, direct, cached)
			}
		}
		if len(memo.facts) != 2 {
			return fmt.Errorf("memo has %d entries, want exactly two shared ancestors", len(memo.facts))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
