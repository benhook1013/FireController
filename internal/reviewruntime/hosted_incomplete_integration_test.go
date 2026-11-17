//go:build hosted_integration

package reviewruntime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// This opt-in test runs the real recovery scheduler and wall clock against a
// controlled provider. It posts exactly one original trigger through the driver,
// observes a finished ACK with no inventory, waits the actual grace, then checks
// durable terminal state. No live provider, clock replacement or fixture aging.
func TestHostedIncompleteRealGraceScheduler(t *testing.T) {
	f, a, p := schedulerUncertainFixture(t)
	f.hosted.wait = nil // Restore the real wait; the ordinary fixture skips timers.
	query, err := f.engine.ReadHostedAllowanceQuery(t.Context(), f.project, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	p.alterSnapshot = func(s *reviewprovider.HostedPublicationSnapshot) {
		unsupportedFinishedSnapshot(s, query.ProviderActor, 200)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 165*time.Second)
	defer cancel()
	done, err := f.hosted.StartRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("real grace did not release within bounded observation window")
		case <-ticker.C:
			var state, reason string
			if err := f.store.DB().QueryRow(`SELECT state,reason FROM review_operations WHERE id=?`, a.OperationID).Scan(&state, &reason); err != nil {
				t.Fatal(err)
			}
			if state != "failed" {
				continue
			}
			if reason != reviewexec.HostedFinishedIncompleteReason {
				t.Fatal("unexpected terminal outcome", reason)
			}
			cancel()
			schedulerDone(t, done, false)
			if time.Since(started) < reviewexec.HostedIncompleteGrace {
				t.Fatal("released before actual grace")
			}
			if f.provider.queries != 1 || f.provider.reviews != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
				t.Fatal("unexpected provider calls, credit or reservation")
			}
			var body string
			if err := f.store.DB().QueryRow(`SELECT payload_json FROM events WHERE event_type='review.hosted_incomplete_released' AND resource_id=?`, a.ID).Scan(&body); err != nil {
				t.Fatal(err)
			}
			var audit struct {
				First    string `json:"first_observed_at"`
				Released string `json:"released_at"`
			}
			if json.Unmarshal([]byte(body), &audit) != nil {
				t.Fatal("invalid release audit")
			}
			first, e1 := time.Parse(time.RFC3339Nano, audit.First)
			released, e2 := time.Parse(time.RFC3339Nano, audit.Released)
			if e1 != nil || e2 != nil || released.Sub(first) < reviewexec.HostedIncompleteGrace {
				t.Fatal("audit lacks real grace")
			}
			t.Logf("Actual elapsed=%s, durable grace=%s, observations=%d, query/review posts=%d/%d, completed evidence=0", time.Since(started), released.Sub(first), f.provider.observations, f.provider.queries, f.provider.reviews)
			return
		}
	}
}
