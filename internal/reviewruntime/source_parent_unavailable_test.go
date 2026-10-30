package reviewruntime

import (
	"errors"
	"github.com/benhook1013/FireController/internal/core"
	"reflect"
	"strings"
	"testing"
)

func TestAncestorUnavailableParentRetainsCheckedChildBranch(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f := newCLIDriverFixture(t)
			child := f.reader.pulls[4]
			child.HeadSHA, child.BaseSHA, child.BaseRef = strings.Repeat("9", 40), f.headSHA, "parent-branch"
			f.reader.pulls[4] = child
			parent := f.reader.pulls[2]
			parent.HeadRef, parent.BaseRef = "parent-branch", "former-parent"
			f.reader.pulls[2] = parent
			ancestor := f.reader.pulls[3]
			ancestor.HeadRef, ancestor.State, ancestor.Merged = "former-parent", "closed", true
			f.reader.pulls[3] = ancestor
			config := sourceConfiguration("stacked", 3, 2, 4)
			operation := sourceOperation(f, 4, channel)
			before := sourceState(t, f)
			plan, _, err := resolveReviewSource(t.Context(), f.engine, newSourceReads(f.reader), 901, config, operation)
			var hold *core.Error
			want := map[string]string{"reason": "parent_unavailable", "pull_number": "2", "base_ref": "former-parent"}
			if !errors.As(err, &hold) || !reflect.DeepEqual(hold.Details, want) || plan.Pull.Number != 4 {
				t.Fatal("ancestor identity lost", plan, err)
			}
			operation.AllowOutOfOrder = true
			if _, _, err = resolveReviewSource(t.Context(), f.engine, newSourceReads(f.reader), 901, config, operation); err != nil {
				t.Fatal("out of order no longer skips ancestor check", err)
			}
			operation.PullNumber, operation.HeadSHA = 2, parent.HeadSHA
			_, _, err = resolveReviewSource(t.Context(), f.engine, newSourceReads(f.reader), 901, config, operation)
			sourceHoldReason(t, err, "parent_unavailable")
			if before != sourceState(t, f) {
				t.Fatal("source diagnostics changed state")
			}
		})
	}
}
