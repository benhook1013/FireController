package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/roundadoptionmodel"
)

func TestRoundAdoptionEventIdentityMatchesSubsequentConfiguration(t *testing.T) {
	f := adoptionFixture(t)
	reader := evidenceReadScope(t, f, attemptProject, f.reader)
	var cursor string
	if err := f.store.Read(t.Context(), reader, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		cursor, err = f.store.ProjectEventCursor(ctx, tx, reader)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	admin := adoptionScope(t, f, f.admin, true)
	p := adoptionPrepared()
	preview, err := f.e.PreviewRoundAdoption(t.Context(), admin, p)
	if err != nil {
		t.Fatal(err)
	}
	request := roundadoptionmodel.ApplyRequest{PreviewSHA256: preview.PreviewSHA256, Control: p}
	original, err := f.e.ApplyRoundAdoption(t.Context(), admin, "event-adopt-cli", request)
	if err != nil {
		t.Fatal(err)
	}
	other := p
	other.PullNumber, other.Channel, other.SourceKey = 4, "hosted", "pr4-hosted"
	adoptRound(t, f, other, "event-adopt-hosted")
	one := int64(1)
	if _, err = f.e.ConfigureRoundControl(t.Context(), admin, "event-close-adopted", 2, "cli", RoundControlRequest{ExpectedRevision: &one, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	beforeReplay := roundTables(t, f)
	replayed, err := f.e.ApplyRoundAdoption(t.Context(), admin, "event-adopt-cli", request)
	if err != nil || string(replayed) != string(original) || roundTables(t, f) != beforeReplay {
		t.Fatal("exact adoption replay changed the later control or added an event", err)
	}
	// Exercise the public project-feed read path; the shared HTTP SSE transport
	// and per-item cursor replay are covered by the round-control event tests.
	stop := errors.New("received bounded project event page")
	var page core.ProjectEventPage
	err = f.store.StreamProjectEvents(t.Context(), reader, cursor, 100, func(value core.ProjectEventPage) error {
		page = value
		return stop
	}, func() error { return errors.New("expected immediate event replay") })
	if !errors.Is(err, stop) || page.HasMore || len(page.Items) != 3 {
		t.Fatalf("expected exactly two adoptions and one update: %+v, %v", page, err)
	}
	wantIDs := []string{"2/cli", "4/hosted", "2/cli"}
	wantRevisions := []int64{1, 1, 2}
	cursors := map[string]bool{cursor: true}
	for i, item := range page.Items {
		if item.EventType != "reviews.rounds_configured" || item.ResourceID != wantIDs[i] || item.Revision != wantRevisions[i] {
			t.Errorf("event %d: want %s revision %d, got %+v", i, wantIDs[i], wantRevisions[i], item)
		}
		if item.EventCursor == "" || cursors[item.EventCursor] {
			t.Errorf("event %d lacks its distinct delivery cursor", i)
		}
		cursors[item.EventCursor] = true
	}
	if roundTables(t, f) != beforeReplay {
		t.Fatal("project event read changed durable state")
	}
	if first, second := roundRead(t, f, 2, "cli"), roundRead(t, f, 4, "hosted"); !first.Closed || first.Revision != 2 || second.Closed || second.Revision != 1 {
		t.Fatal("ordinary closure or old adoption replay changed an unrelated control")
	}
}
