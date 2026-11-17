package reviewruntime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestHostedDriverDispatchUsesFreshSourceWithoutRenewingHistoricalFacts(t *testing.T) {
	f := newHostedDriverFixture(t)
	op, a := f.prepareHosted(t)
	old := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
	in, err := f.engine.ReadHostedAllowance(t.Context(), f.project, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(in)
	in.Anchor.ObservedAt = old
	aged, _ := json.Marshal(in)
	var preflight string
	if err = f.store.DB().QueryRow(`SELECT body_json FROM review_hosted_preflight WHERE attempt_id=?`, a.ID).Scan(&preflight); err != nil {
		t.Fatal(err)
	}
	preflight = strings.Replace(preflight, string(original), string(aged), 1)
	if _, err = f.store.DB().Exec(`UPDATE review_hosted_preflight SET body_json=? WHERE attempt_id=?`, preflight, a.ID); err != nil {
		t.Fatal(err)
	}
	anchor := *a.Anchor
	anchor.ObservedAt = old
	a.Anchor = &anchor
	encodedAnchor, _ := json.Marshal(anchor)
	if _, err = f.store.DB().Exec(`UPDATE review_execution_attempts SET anchor_json=? WHERE id=?`, string(encodedAnchor), a.ID); err != nil {
		t.Fatal(err)
	}
	f.hosted.prepared[a.ID].anchor = anchor
	f.hosted.prepared[a.ID].plan.ObservedAt = old
	// Model aged initial observations without sleeping or changing the fresh
	// provider reads, original allowance response, or immutable fetch timestamp.
	if read, err := f.engine.ReadHostedAllowance(t.Context(), f.project, a.ID); err != nil || read != in || time.Since(read.Observation.ResponseCreatedAt) > time.Minute {
		t.Fatal("aged fixture is not otherwise valid/fresh", err)
	}
	repositoryCalls, pullCalls := f.provider.repositoryCalls, f.provider.calls[2]
	posted := false
	f.provider.postHook = func() {
		if f.provider.reviews == 1 {
			posted = true
			if f.provider.repositoryCalls != repositoryCalls+1 || f.provider.calls[2] != pullCalls+1 {
				t.Error("dispatch added or skipped selected-source provider reads", f.provider.repositoryCalls-repositoryCalls, f.provider.calls[2]-pullCalls)
			}
		}
	}
	result, err := f.hosted.Dispatch(t.Context(), op, a, f.callback(t, &a))
	if err != nil || result.State != "completed" || !posted || f.provider.queries != 1 || f.provider.reviews != 1 {
		t.Fatal("fresh dispatch was refused by historical source age", result, err)
	}
	var afterPreflight, afterAnchor string
	if err = f.store.DB().QueryRow(`SELECT body_json FROM review_hosted_preflight WHERE attempt_id=?`, a.ID).Scan(&afterPreflight); err != nil {
		t.Fatal(err)
	}
	if err = f.store.DB().QueryRow(`SELECT anchor_json FROM review_execution_attempts WHERE id=?`, a.ID).Scan(&afterAnchor); err != nil {
		t.Fatal(err)
	}
	if afterPreflight != preflight || afterAnchor != string(encodedAnchor) {
		t.Fatal("dispatch refreshed immutable source/allowance records")
	}
	if result, err = f.hosted.Dispatch(t.Context(), op, a, f.callback(t, &a)); err != nil || result.State != "failed" || f.provider.reviews != 1 {
		t.Fatal("replay granted another post", result, err)
	}
}
