package reviewexec

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/server"
)

func TestRunSelectionPausedAndUnconfiguredAdmissionErrors(t *testing.T) {
	for _, test := range []struct {
		name        string
		configured  bool
		paused      bool
		policyPause bool
		code        string
	}{
		{"unconfigured channel", false, false, false, "review_not_configured"},
		{"paused project", true, true, false, "reviews_paused"},
		{"paused project and unconfigured channel", false, true, false, "reviews_paused"},
		{"policy paused channel", true, false, true, "reviews_paused"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := selectionFixture(t)
			channels := []string{"cli"}
			if test.configured {
				channels = append(channels, "hosted")
			}
			previous := int64(1)
			_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "admission-configuration", Configuration{ExpectedRevision: &previous, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4}, Channels: channels, Paused: test.paused})
			if err != nil {
				t.Fatal(err)
			}
			if test.policyPause {
				admissionPolicy(t, f, 1, "hosted")
			}
			before := selectionCounts(t, f) + roundTables(t, f)
			scope := selectionScope(t, f, attemptProject, f.writer, "write")
			_, selectionErr := f.e.EnqueueRun(t.Context(), scope, "refused-selection", RunRequest{Channel: "hosted"})
			_, requestErr := f.e.Enqueue(t.Context(), scope, "refused-request", Request{ExpectedConfigurationRevision: 2, PullNumber: 2, Channel: "hosted", HeadSHA: strings.Repeat("a", 40)})
			for name, err := range map[string]error{"selection": selectionErr, "exact request": requestErr} {
				var failure *core.Error
				if !errors.As(err, &failure) || failure.Code != test.code || failure.Status != http.StatusConflict {
					t.Errorf("%s: want %s/409, got %v", name, test.code, err)
				}
			}
			if after := selectionCounts(t, f) + roundTables(t, f); after != before {
				t.Fatal("refused admission changed durable selections, operations, rounds, events or receipts")
			}
		})
	}
}

func TestRoundControlProjectEventResourcesAndCursorReplay(t *testing.T) {
	f := roundFixture(t)
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	var cursor string
	if err := f.store.Read(t.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		cursor, err = f.store.ProjectEventCursor(ctx, tx, scope)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	roundGrant(t, f, 2, "cli", 0, 0, 8, "event-cli-2")
	roundGrant(t, f, 2, "hosted", 0, 0, 5, "event-hosted-2")
	roundGrant(t, f, 4, "cli", 0, 0, 8, "event-cli-4")
	revision := int64(1)
	closeRequest := RoundControlRequest{ExpectedRevision: &revision, Action: "close"}
	admin := f.scope(t, attemptProject, f.admin, true)
	first, err := f.e.ConfigureRoundControl(t.Context(), admin, "event-close", 2, "cli", closeRequest)
	if err != nil {
		t.Fatal(err)
	}
	beforeReplay := roundTables(t, f)
	replayed, err := f.e.ConfigureRoundControl(t.Context(), admin, "event-close", 2, "cli", closeRequest)
	if err != nil || string(replayed) != string(first) || roundTables(t, f) != beforeReplay {
		t.Fatal("exact mutation replay changed receipt or appended an event", err)
	}
	mux := http.NewServeMux()
	server.RegisterProjectStream(mux, f.store, t.Context())
	s := httptest.NewServer(mux)
	defer s.Close()
	readPage := func(after string, limit string) core.ProjectEventPage {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+"/api/v1/projects/"+attemptProject+"/project-events/stream?cursor="+url.QueryEscape(after)+"&limit="+limit, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+f.reader)
		response, err := s.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("project stream status/content type: %d %s", response.StatusCode, response.Header.Get("Content-Type"))
		}
		scanner := bufio.NewScanner(response.Body)
		var event, id, data string
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				break
			}
			if value, ok := strings.CutPrefix(line, "event: "); ok {
				event = value
			} else if value, ok := strings.CutPrefix(line, "id: "); ok {
				id = value
			} else if value, ok := strings.CutPrefix(line, "data: "); ok {
				data = value
			}
		}
		var page core.ProjectEventPage
		if scanner.Err() != nil || event != "project-events" || id == "" || json.Unmarshal([]byte(data), &page) != nil || page.EventCursor != id {
			t.Fatalf("invalid public project event frame: %q %q %q, %v", event, id, data, scanner.Err())
		}
		return page
	}
	firstPage := readPage(cursor, "2")
	if !firstPage.HasMore || len(firstPage.Items) != 2 {
		t.Fatalf("first bounded page: %+v", firstPage)
	}
	secondPage := readPage(firstPage.EventCursor, "2")
	if secondPage.HasMore || len(secondPage.Items) != 2 {
		t.Fatalf("second bounded page: %+v", secondPage)
	}
	items := append(firstPage.Items, secondPage.Items...)
	wantIDs := []string{"2/cli", "2/hosted", "4/cli", "2/cli"}
	wantRevisions := []int64{1, 1, 1, 2}
	seen := map[string]bool{cursor: true}
	for i, item := range items {
		if item.EventType != "reviews.rounds_configured" || item.ResourceID != wantIDs[i] || item.Revision != wantRevisions[i] {
			t.Errorf("event %d: want resource %s revision %d, got %+v", i, wantIDs[i], wantRevisions[i], item)
		}
		if item.EventCursor == "" || seen[item.EventCursor] {
			t.Errorf("event %d lacks its own durable delivery cursor", i)
		}
		seen[item.EventCursor] = true
	}
	gapReplay := readPage(items[0].EventCursor, "100")
	if gapReplay.HasMore || !reflect.DeepEqual(gapReplay.Items, items[1:]) {
		t.Fatal("replay from an item cursor changed or lost later public event envelopes")
	}
	if roundTables(t, f) != beforeReplay {
		t.Fatal("public event reads changed durable state")
	}
}
