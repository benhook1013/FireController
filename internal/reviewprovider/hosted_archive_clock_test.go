package reviewprovider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func archiveClockDiscussion() IssueComment {
	created, _ := time.Parse(time.RFC3339, "2026-11-01T09:36:10Z")
	updated, _ := time.Parse(time.RFC3339, "2026-11-01T09:05:21Z")
	return IssueComment{ID: 900, Author: Author{ID: 90, Login: "github-actions[bot]", Kind: "Bot"}, Body: "Synthetic validation status: pending.", CreatedAt: created, UpdatedAt: updated}
}

func TestExternalHostedArchiveUnrelatedClockDoesNotPoisonProviderInventory(t *testing.T) {
	f := externalFixture(t)
	body, err := HostedTriggerBody(f.intent)
	if err != nil {
		t.Fatal(err)
	}
	command := IssueComment{ID: 301, Author: f.poster, Body: body, CreatedAt: f.earliest, UpdatedAt: f.earliest}
	f.snapshot.IssueComments = []IssueComment{command}
	f.snapshot.ObservedAt, _ = time.Parse(time.RFC3339, "2026-11-01T11:15:18Z")
	original := externalDiscover(t, f)
	if len(original) != 1 || !original[0].Inventory.Complete || original[0].TriggerCommentID != command.ID {
		t.Fatal("fixture lacks real inventory and unique native hint", original)
	}
	for _, mode := range []string{"reversed unrelated interval", "future unrelated interval"} {
		t.Run(mode, func(t *testing.T) {
			snapshot := f.snapshot
			discussion := archiveClockDiscussion()
			if mode == "future unrelated interval" {
				discussion.CreatedAt = snapshot.ObservedAt.Add(time.Second)
				discussion.UpdatedAt = discussion.CreatedAt
			}
			snapshot.IssueComments = append([]IssueComment{command}, discussion)
			before := fixtureJSON(t, snapshot)
			got, err := DiscoverExternalHosted(snapshot, f.bot)
			if err != nil || len(got) != 1 {
				t.Fatal("unrelated chatter poisoned actual provider inventory", err)
			}
			want := original[0]
			want.TriggerCommentID, want.NativeOperationID = 0, ""
			if !reflect.DeepEqual(got[0], want) || !reflect.DeepEqual(before, fixtureJSON(t, snapshot)) {
				t.Fatal("native hint claimed unknown chronology or source facts changed", got)
			}
			if validPublicationSnapshot(snapshot) {
				t.Fatal("native publication validation was relaxed")
			}
		})
	}
}

func TestExternalHostedArchiveRelevantClockAndMalformedRowsRemainInvalid(t *testing.T) {
	for _, mode := range []string{"provider reversed", "provider future", "command reversed", "command future", "duplicate identity", "invalid actor", "zero timestamp"} {
		t.Run(mode, func(t *testing.T) {
			f := externalFixture(t)
			f.snapshot.ObservedAt, _ = time.Parse(time.RFC3339, "2026-11-01T11:15:18Z")
			row := archiveClockDiscussion()
			switch mode {
			case "provider reversed":
				row.Author = f.bot
			case "provider future":
				row.Author = f.bot
				row.CreatedAt = f.snapshot.ObservedAt.Add(time.Second)
				row.UpdatedAt = row.CreatedAt
			case "command reversed":
				row.Body = "@coderabbitai full review"
			case "command future":
				row.Body = "@coderabbitai full review"
				row.CreatedAt = f.snapshot.ObservedAt.Add(time.Second)
				row.UpdatedAt = row.CreatedAt
			case "duplicate identity":
				f.snapshot.IssueComments = []IssueComment{row}
			case "invalid actor":
				row.Author.ID = 0
			case "zero timestamp":
				row.CreatedAt = time.Time{}
			}
			f.snapshot.IssueComments = append(f.snapshot.IssueComments, row)
			got, err := DiscoverExternalHosted(f.snapshot, f.bot)
			if !hasCode(err, "provider_invalid_response") || len(got) != 0 {
				t.Fatal("untrusted chronology or malformed source supplied inventory", mode, err)
			}
		})
	}
}

func TestObserveHostedArchiveScopesCommentClocksAndPreservesRawFacts(t *testing.T) {
	for _, mode := range []string{"unrelated reversed", "unrelated future", "provider reversed", "command reversed", "duplicate", "malformed time", "invalid actor", "invalid provider"} {
		t.Run(mode, func(t *testing.T) {
			discussion := archiveClockDiscussion()
			provider := Author{ID: 20, Login: "coderabbitai[bot]", Kind: "Bot"}
			if mode == "unrelated future" {
				discussion.CreatedAt = time.Now().UTC().Add(time.Hour)
				discussion.UpdatedAt = discussion.CreatedAt
			}
			if mode == "provider reversed" {
				discussion.Author = provider
			}
			if mode == "command reversed" {
				discussion.Body = "@coderabbitai full review"
			}
			if mode == "invalid actor" {
				discussion.Author.ID = 0
			}
			raw := map[string]any{"id": discussion.ID, "body": discussion.Body, "user": map[string]any{"id": discussion.Author.ID, "login": discussion.Author.Login, "type": discussion.Author.Kind}, "created_at": discussion.CreatedAt.Format(time.RFC3339Nano), "updated_at": discussion.UpdatedAt.Format(time.RFC3339Nano)}
			if mode == "malformed time" {
				raw["updated_at"] = "not-a-timestamp"
			}
			rows := []any{raw}
			if mode == "duplicate" {
				rows = append(rows, raw)
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" || r.URL.Query().Get("page") != "1" || r.URL.Query().Get("per_page") != "100" {
					t.Error("archive observation changed read-only bounded collection")
				}
				switch r.URL.Path {
				case "/repos/owner/repo/pulls/42/reviews", "/repos/owner/repo/pulls/42/comments":
					_ = json.NewEncoder(w).Encode([]any{})
				case "/repos/owner/repo/issues/42/comments":
					_ = json.NewEncoder(w).Encode(rows)
				default:
					t.Error("unexpected target")
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			if mode == "invalid provider" {
				provider.ID = 0
			}
			adapter := testGitHub(server, "synthetic-replay")
			got, err := adapter.ObserveHostedArchive(t.Context(), "owner/repo", 42, provider)
			if mode == "invalid provider" {
				if !hasCode(err, "provider_invalid_response") || calls != 0 {
					t.Fatal("invalid actor made requests", err, calls)
				}
				return
			}
			if mode != "unrelated reversed" && mode != "unrelated future" {
				if !hasCode(err, "provider_invalid_response") || got.Complete || got.IssueComments != nil || calls != 3 {
					t.Fatal("selected or malformed evidence became usable", err, got.Complete, calls)
				}
				return
			}
			if err != nil || !got.Complete || len(got.IssueComments) != 1 || !reflect.DeepEqual(got.IssueComments[0], discussion) || got.Reviews == nil || got.ReviewComments == nil || calls != 3 {
				t.Fatal("archive chatter clock was repaired, dropped or rejected", err, got.Complete, calls)
			}
			runs, err := DiscoverExternalHosted(got, provider)
			if err != nil || len(runs) != 0 {
				t.Fatal("chatter supplied a synthetic zero/completion", err, runs)
			}
			native, err := adapter.ObserveHosted(t.Context(), "owner/repo", 42)
			if !hasCode(err, "provider_invalid_response") || native.Complete || native.IssueComments != nil || calls != 6 {
				t.Fatal("strict native observation accepted bad clocks", err, calls)
			}
		})
	}
}
