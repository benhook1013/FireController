package reviewruntime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

func TestCLIDriverPersistsSpawnBeforeReviewReturns(t *testing.T) {
	f := newCLIDriverFixture(t)
	op, a := f.prepareAdmit(t, 2, 1, "spawn-live")
	original := f.driver.review
	f.driver.review = func(ctx context.Context, w reviewworkspace.Workspace) (reviewprocess.Capture, error) {
		onStart, ok := ctx.Value(cliStartCallbackKey{}).(func(time.Time))
		if !ok {
			t.Fatal("live callback not supplied")
		}
		at := time.Now().UTC()
		onStart(at)
		var body string
		if err := f.store.DB().QueryRow(`SELECT body_json FROM review_activity_evidence WHERE attempt_id=?`, a.ID).Scan(&body); err != nil {
			t.Fatal("spawn not retained during review", err)
		}
		var v reviewexec.ActivityEvidence
		if json.Unmarshal([]byte(body), &v) != nil || v.AttemptID != a.ID || v.HeadSHA != op.HeadSHA || v.ProcessStartedAt != at.Format(time.RFC3339Nano) {
			t.Fatal("wrong live evidence", body)
		}
		return original(ctx, w)
	}
	result, err := f.driver.Dispatch(t.Context(), op, a, nil)
	if err != nil || result.State != "completed" {
		t.Fatal(result, err)
	}
}
