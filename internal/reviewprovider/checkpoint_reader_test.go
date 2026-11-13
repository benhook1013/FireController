package reviewprovider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func checkpointWire(c IssueComment) githubIssueComment {
	return githubIssueComment{ID: c.ID, Body: c.Body, User: githubActor{ID: c.Author.ID, Login: c.Author.Login, Type: c.Author.Kind}, CreatedAt: c.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: c.UpdatedAt.Format(time.RFC3339Nano)}
}

func TestCheckpointCommentsForeignContentAndSharedReaderIsolation(t *testing.T) {
	p := checkpointFixture()
	own := checkpointComment(t, 31, p)
	foreign := checkpointWire(checkpointComment(t, 32, p))
	foreign.User.ID, foreign.User.Login = 99, "bad login"
	foreign.Body, foreign.CreatedAt, foreign.UpdatedAt = "foreign\x00\x1b", "unusable", "unusable"
	wrongKind := foreign
	wrongKind.ID, wrongKind.User.ID, wrongKind.User.Type = 34, own.Author.ID, "Bot"
	// Every attributable comment is returned, including unrelated prose, so the
	// collector cannot hide a second conflicting marker from the pure matcher.
	other := checkpointComment(t, 33, p)
	other.Body = "Unrelated but attributable."
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/repos/owner/repo/issues/7/comments" || r.URL.Query().Get("per_page") != "100" {
			t.Error("checkpoint collection changed scope or method")
		}
		_ = json.NewEncoder(w).Encode([]githubIssueComment{foreign, wrongKind, checkpointWire(own), checkpointWire(other)})
	}))
	defer server.Close()
	g := testGitHub(server, "")
	got, err := g.CheckpointComments(t.Context(), "owner/repo", 7, checkpointActor())
	if err != nil || !reflect.DeepEqual(got, []IssueComment{own, other}) || calls.Load() != 1 {
		t.Fatal("foreign cosmetics blocked checkpoint projection", len(got), err, calls.Load())
	}
	match, found, err := FindCheckpoint(p, checkpointActor(), got)
	if err != nil || !found || match != own {
		t.Fatal("collector did not preserve original match", found, err)
	}
	if got, err := g.IssueComments(t.Context(), "owner/repo", 7); !hasCode(err, "provider_invalid_response") || got != nil {
		t.Fatal("shared strict reader was relaxed", err)
	}
}

func TestCheckpointCommentsIdentityAndAttributableValidation(t *testing.T) {
	own := checkpointWire(checkpointComment(t, 31, checkpointFixture()))
	foreign := own
	foreign.ID, foreign.User.ID = 32, 99
	for name, mutate := range map[string]func(*githubIssueComment){
		"foreign_zero_comment":      func(c *githubIssueComment) { c.ID = 0 },
		"foreign_duplicate_comment": func(c *githubIssueComment) { c.ID = own.ID },
		"foreign_zero_actor":        func(c *githubIssueComment) { c.User.ID = 0 },
		"foreign_empty_kind":        func(c *githubIssueComment) { c.User.Type = "" },
		"foreign_invalid_kind":      func(c *githubIssueComment) { c.User.Type = "bad kind" },
		"own_body":                  func(c *githubIssueComment) { *c = own; c.ID = 32; c.Body += "\x00" },
		"own_login":                 func(c *githubIssueComment) { *c = own; c.ID = 32; c.User.Login = "bad login" },
		"own_created":               func(c *githubIssueComment) { *c = own; c.ID = 32; c.CreatedAt = "bad" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := foreign
			mutate(&bad)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("unexpected write")
				}
				_ = json.NewEncoder(w).Encode([]githubIssueComment{own, bad})
			}))
			defer server.Close()
			got, err := testGitHub(server, "").CheckpointComments(t.Context(), "owner/repo", 7, checkpointActor())
			if !hasCode(err, "provider_invalid_response") || got != nil {
				t.Fatal("invalid inventory returned partial projection", len(got), err)
			}
		})
	}
}

func TestCheckpointCommentsCompletePaginationAndConflicts(t *testing.T) {
	p := checkpointFixture()
	own := checkpointComment(t, 31, p)
	for _, conflict := range []bool{false, true} {
		t.Run(strconv.FormatBool(conflict), func(t *testing.T) {
			second := checkpointComment(t, 32, p)
			if conflict {
				second.Body = strings.Replace(second.Body, "3 Found", "99 Found", 1)
			}
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				comment := own
				if r.URL.Query().Get("page") == "1" {
					w.Header().Set("Link", "<"+serverURL(r)+r.URL.Path+"?per_page=100&page=2>; rel=next")
				} else {
					comment = second
				}
				_ = json.NewEncoder(w).Encode([]githubIssueComment{checkpointWire(comment)})
			}))
			defer server.Close()
			got, err := testGitHub(server, "").CheckpointComments(t.Context(), "owner/repo", 7, checkpointActor())
			if err != nil || len(got) != 2 || calls.Load() != 2 {
				t.Fatal("incomplete checkpoint history", len(got), err, calls.Load())
			}
			_, found, err := FindCheckpoint(p, checkpointActor(), got)
			code := "checkpoint_ambiguous"
			if conflict {
				code = "checkpoint_conflict"
			}
			if found || !hasCode(err, code) {
				t.Fatal("attributable conflict lost", found, err)
			}
		})
	}
}

func TestCheckpointCommentsForeignControlsDoNotRelaxSharedEvidence(t *testing.T) {
	own := checkpointWire(checkpointComment(t, 31, checkpointFixture()))
	for _, body := range []string{"foreign\x00", "foreign\x1b"} {
		t.Run(strconv.Quote(body), func(t *testing.T) {
			foreign := own
			foreign.ID, foreign.User.ID, foreign.Body = 32, 99, body
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode([]githubIssueComment{own, foreign})
			}))
			defer server.Close()
			g := testGitHub(server, "")
			if got, err := g.CheckpointComments(t.Context(), "owner/repo", 7, checkpointActor()); err != nil || len(got) != 1 || got[0].ID != own.ID {
				t.Fatal("foreign control blocked isolated projection", len(got), err)
			}
			if got, err := g.IssueComments(t.Context(), "owner/repo", 7); got != nil || !hasCode(err, "provider_invalid_response") {
				t.Fatal("shared body-control guard relaxed", err)
			}
		})
	}
}

func TestCheckpointCommentsSelectorsAndEmptyProjection(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		foreign := checkpointWire(checkpointComment(t, 31, checkpointFixture()))
		foreign.User.ID = 99
		_ = json.NewEncoder(w).Encode([]githubIssueComment{foreign})
	}))
	defer server.Close()
	g := testGitHub(server, "")
	for _, actor := range []Author{{}, {ID: 17, Kind: "User", Login: "bad login"}, {ID: 17, Login: "original", Kind: ""}} {
		if got, err := g.CheckpointComments(t.Context(), "owner/repo", 7, actor); got != nil || !hasCode(err, "provider_invalid_response") {
			t.Fatal("invalid expected actor dispatched", err)
		}
	}
	for _, selector := range []struct {
		repository string
		number     int64
	}{{"../foreign", 7}, {"owner/repo", 0}} {
		if _, err := g.CheckpointComments(t.Context(), selector.repository, selector.number, checkpointActor()); !hasCode(err, "provider_invalid_response") {
			t.Fatal("invalid target accepted", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid selector made HTTP request")
	}
	got, err := g.CheckpointComments(t.Context(), "owner/repo", 7, checkpointActor())
	if err != nil || got == nil || len(got) != 0 || calls.Load() != 1 {
		t.Fatal("empty projection was not complete", len(got), err)
	}
}

func TestCheckpointCommentsCollectionFailuresNeverReturnPartialMatches(t *testing.T) {
	own := checkpointWire(checkpointComment(t, 31, checkpointFixture()))
	for _, scenario := range []string{"page_failure", "foreign_duplicate_on_later_page", "bad_link", "items", "aggregate", "page_bytes", "null_body", "duplicate_json", "malformed_json"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/repos/owner/repo/issues/7/comments" {
					t.Error("changed collection scope or method")
				}
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				next := func() {
					w.Header().Set("Link", "<"+serverURL(r)+r.URL.Path+"?page="+strconv.Itoa(page+1)+"&per_page=100>; rel=next")
				}
				switch scenario {
				case "page_failure", "foreign_duplicate_on_later_page":
					if page == 1 {
						next()
						_ = json.NewEncoder(w).Encode([]githubIssueComment{own})
						return
					}
					if scenario == "page_failure" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					foreign := own
					foreign.User.ID, foreign.Body = 99, "foreign\x00"
					_ = json.NewEncoder(w).Encode([]githubIssueComment{foreign})
				case "bad_link":
					w.Header().Set("Link", "<https://foreign.invalid/private?page=2&per_page=100>; rel=next")
					_ = json.NewEncoder(w).Encode([]githubIssueComment{own})
				case "items":
					items := make([]githubIssueComment, githubPageSize+1)
					for i := range items {
						items[i] = own
						items[i].ID = int64(i + 1)
						items[i].User.ID = 99
					}
					_ = json.NewEncoder(w).Encode(items)
				case "aggregate":
					if page < 4 {
						next()
					}
					foreign := own
					foreign.ID, foreign.User.ID, foreign.Body = int64(page), 99, strings.Repeat("x", githubMaxAggregate/4)
					_ = json.NewEncoder(w).Encode([]githubIssueComment{foreign})
				case "page_bytes":
					_, _ = w.Write([]byte("[" + strings.Repeat(" ", githubMaxBody) + "]"))
				case "null_body":
					raw, _ := json.Marshal(own)
					var object map[string]any
					_ = json.Unmarshal(raw, &object)
					object["body"] = nil
					object["user"].(map[string]any)["id"] = 99
					_ = json.NewEncoder(w).Encode([]any{object})
				case "duplicate_json":
					raw, _ := json.Marshal(own)
					_, _ = w.Write([]byte("[" + strings.Replace(string(raw), `"id":31`, `"id":31,"id":32`, 1) + "]"))
				case "malformed_json":
					_, _ = w.Write([]byte("[{"))
				}
			}))
			defer server.Close()
			got, err := testGitHub(server, "").CheckpointComments(t.Context(), "owner/repo", 7, checkpointActor())
			code := "provider_invalid_response"
			if scenario == "page_failure" {
				code = "provider_unavailable"
			}
			if got != nil || !hasCode(err, code) {
				t.Fatal("incomplete collection exposed projected match", len(got), err)
			}
			wantCalls := int64(1)
			if scenario == "page_failure" || scenario == "foreign_duplicate_on_later_page" {
				wantCalls = 2
			}
			if scenario == "aggregate" {
				wantCalls = 4
			}
			if calls.Load() != wantCalls {
				t.Fatal("unexpected retries or foreign pagination", calls.Load(), wantCalls)
			}
		})
	}
}
