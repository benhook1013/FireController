package reviewprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func parentCandidateFixture(ref string) map[string]any {
	return map[string]any{"number": int64(2), "state": "open", "head": map[string]any{"ref": ref, "repo": map[string]any{"id": int64(7), "full_name": "owner/repo"}}, "base": map[string]any{"repo": map[string]any{"id": int64(7), "full_name": "owner/repo"}}}
}
func parentFreshFixture(ref string) map[string]any {
	repo := func() map[string]any {
		return map[string]any{"id": int64(7), "full_name": "owner/repo", "default_branch": "main"}
	}
	return map[string]any{"number": int64(2), "title": "Actual parent", "state": "open", "merged": false, "head": map[string]any{"ref": ref, "sha": strings.Repeat("c", 40), "repo": repo()}, "base": map[string]any{"ref": "main", "sha": strings.Repeat("d", 40), "repo": repo()}, "changed_files": int64(4)}
}
func parentObject(v map[string]any, key string) map[string]any { return v[key].(map[string]any) }

func TestParentPullUniqueFreshIdentityEscapingAndConditionalReads(t *testing.T) {
	ref := "feature/parent+phase&x=1"
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" {
			t.Error("parent lookup mutated provider")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/repos/owner/repo/pulls" {
			query := r.URL.Query()
			if len(query) != 4 || query.Get("head") != "owner:"+ref || query.Get("state") != "open" || query.Get("per_page") != "2" || query.Get("page") != "1" {
				t.Errorf("unbounded or changed selectors: %v", query)
			}
			for _, part := range []string{"%2F", "%2B", "%26", "%3D"} {
				if !strings.Contains(r.URL.RawQuery, part) {
					t.Errorf("selector not escaped: %s", part)
				}
			}
			w.Header().Set("ETag", `"candidate"`)
			if r.Header.Get("If-None-Match") == `"candidate"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			json.NewEncoder(w).Encode([]any{parentCandidateFixture(ref)})
			return
		}
		if r.URL.Path != "/repos/owner/repo/pulls/2" || r.URL.RawQuery != "" {
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", `"fresh-parent"`)
		if r.Header.Get("If-None-Match") == `"fresh-parent"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		json.NewEncoder(w).Encode(parentFreshFixture(ref))
	}))
	defer server.Close()
	g := testGitHub(server, "parent-fixture")
	for i := 0; i < 2; i++ {
		pull, err := g.ParentPull(t.Context(), "owner/repo", ref)
		if err != nil || pull.Number != 2 || pull.RepositoryID != 7 || pull.State != "open" || pull.Merged || pull.HeadRef != ref || pull.HeadSHA != strings.Repeat("c", 40) || pull.BaseSHA != strings.Repeat("d", 40) || pull.BaseRef != "main" {
			t.Fatal("unique fresh parent lost exact facts", pull, err)
		}
	}
	if calls.Load() != 4 {
		t.Fatal("parent reused cache without revalidation or performed extra reads", calls.Load())
	}
}

func TestParentPullEmptyAmbiguousAndMalformedInventory(t *testing.T) {
	ref := "feature/parent"
	cases := []struct {
		name, body, link string
		mutate           func(map[string]any)
		zero             bool
		ambiguous        bool
	}{
		{name: "zero", body: "[]", zero: true},
		{name: "ambiguous", body: "two", ambiguous: true},
		{name: "too many", body: "three"},
		{name: "null", body: "null"},
		{name: "object", body: "{}"},
		{name: "malformed", body: "["},
		{name: "duplicate key", body: `[{"number":2,"number":2}]`},
		{name: "foreign pagination", link: `<https://foreign.invalid/pulls?page=2>; rel="next"`},
		{name: "pagination", link: "valid", ambiguous: true},
		{name: "partial empty inventory", body: "[]", link: "valid", ambiguous: true},
		{name: "changed pagination selector", link: "changed-selector"},
		{name: "malformed matching candidates", body: "malformed-two"},
		{name: "malformed pagination", link: "not-a-link"},
		{name: "duplicate candidates", body: "duplicate"},
		{name: "malformed candidate with pagination", body: "[", link: "valid"},
		{name: "empty pagination", link: " "},
		{name: "foreign head", mutate: func(v map[string]any) { parentObject(parentObject(v, "head"), "repo")["full_name"] = "other/repo" }},
		{name: "same owner different repo", mutate: func(v map[string]any) { parentObject(parentObject(v, "head"), "repo")["full_name"] = "owner/fork" }},
		{name: "foreign base", mutate: func(v map[string]any) { parentObject(parentObject(v, "base"), "repo")["full_name"] = "other/repo" }},
		{name: "different head id", mutate: func(v map[string]any) { parentObject(parentObject(v, "head"), "repo")["id"] = 8 }},
		{name: "unproven head", mutate: func(v map[string]any) { parentObject(v, "head")["repo"] = nil }},
		{name: "missing head repo", mutate: func(v map[string]any) { delete(parentObject(v, "head"), "repo") }},
		{name: "wrong branch", mutate: func(v map[string]any) { parentObject(v, "head")["ref"] = "feature/other" }},
		{name: "closed candidate", mutate: func(v map[string]any) { v["state"] = "closed" }},
		{name: "zero candidate number", mutate: func(v map[string]any) { v["number"] = 0 }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/repos/owner/repo/pulls" {
					t.Error("unproven inventory caused fresh lookup")
				}
				if test.link != "" {
					link := test.link
					if link == "valid" || link == "changed-selector" {
						query := url.Values{"state": {"open"}, "head": {"owner:" + ref}, "per_page": {"2"}, "page": {"2"}}
						if link == "changed-selector" {
							query.Set("head", "owner:other-branch")
						}
						link = "<" + serverURL(r) + r.URL.Path + "?" + query.Encode() + ">; rel=\"next\""
					}
					w.Header()["Link"] = []string{link}
				}
				w.Header().Set("Content-Type", "application/json")
				if test.body == "two" || test.body == "three" || test.body == "duplicate" || test.body == "malformed-two" {
					rows := []any{parentCandidateFixture(ref), parentCandidateFixture(ref)}
					if test.body != "duplicate" {
						rows[1].(map[string]any)["number"] = 3
					}
					if test.body == "malformed-two" {
						rows[1].(map[string]any)["state"] = "closed"
					}
					if test.body == "three" {
						rows = append(rows, parentCandidateFixture(ref))
					}
					json.NewEncoder(w).Encode(rows)
					return
				}
				if test.body != "" {
					fmt.Fprint(w, test.body)
					return
				}
				candidate := parentCandidateFixture(ref)
				if test.mutate != nil {
					test.mutate(candidate)
				}
				json.NewEncoder(w).Encode([]any{candidate})
			}))
			defer server.Close()
			pull, err := testGitHub(server, "fixture").ParentPull(t.Context(), "owner/repo", ref)
			if test.zero {
				if err != nil || pull != (Pull{}) {
					t.Fatal("empty inventory is not a parent", pull, err)
				}
			} else if test.ambiguous {
				if !errors.Is(err, ErrParentAmbiguous) || pull != (Pull{}) {
					t.Fatal("unproven uniqueness lost typed hold", pull, err)
				}
			} else if !hasCode(err, "provider_invalid_response") || errors.Is(err, ErrParentAmbiguous) || pull != (Pull{}) {
				t.Fatal("unproven inventory accepted", pull, err)
			}
			if calls.Load() != 1 {
				t.Fatal("inventory triggered further provider work", calls.Load())
			}
		})
	}
}

func TestParentPullFreshMovedDeletedAndForeignIdentityRefused(t *testing.T) {
	ref := "feature/parent"
	cases := []struct {
		name   string
		status int
		mutate func(map[string]any)
	}{
		{name: "moved branch", mutate: func(v map[string]any) { parentObject(v, "head")["ref"] = "feature/moved" }},
		{name: "missing fresh branch", mutate: func(v map[string]any) { delete(parentObject(v, "head"), "ref") }},
		{name: "deleted head repository", mutate: func(v map[string]any) { parentObject(v, "head")["repo"] = nil }},
		{name: "deleted pull", status: 404},
		{name: "closed", mutate: func(v map[string]any) { v["state"] = "closed" }},
		{name: "merged", mutate: func(v map[string]any) { v["state"] = "closed"; v["merged"] = true }},
		{name: "different pull", mutate: func(v map[string]any) { v["number"] = 3 }},
		{name: "foreign head", mutate: func(v map[string]any) { parentObject(parentObject(v, "head"), "repo")["full_name"] = "owner/fork" }},
		{name: "different head id", mutate: func(v map[string]any) { parentObject(parentObject(v, "head"), "repo")["id"] = 8 }},
		{name: "foreign base", mutate: func(v map[string]any) { parentObject(parentObject(v, "base"), "repo")["full_name"] = "other/repo" }},
		{name: "base identity changed", mutate: func(v map[string]any) { parentObject(parentObject(v, "base"), "repo")["id"] = 8 }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/repos/owner/repo/pulls" {
					json.NewEncoder(w).Encode([]any{parentCandidateFixture(ref)})
					return
				}
				if r.URL.Path != "/repos/owner/repo/pulls/2" {
					t.Error("unexpected provider path")
					http.NotFound(w, r)
					return
				}
				if test.status != 0 {
					w.WriteHeader(test.status)
					fmt.Fprint(w, `{"message":"Not found"}`)
					return
				}
				fresh := parentFreshFixture(ref)
				test.mutate(fresh)
				json.NewEncoder(w).Encode(fresh)
			}))
			defer server.Close()
			pull, err := testGitHub(server, "fixture").ParentPull(t.Context(), "owner/repo", ref)
			if err == nil || pull != (Pull{}) {
				t.Fatal("changed parent accepted", pull, err)
			}
			if test.status == 0 && !hasCode(err, "provider_invalid_response") {
				t.Fatal(err)
			}
			if calls.Load() != 2 {
				t.Fatal("parent retried or used extra provider reads", calls.Load())
			}
		})
	}
}

func TestParentPullSelectorsAndCancellation(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, "[]") }))
	defer server.Close()
	g := testGitHub(server, "fixture")
	for _, ref := range []string{"", " ", "bad\nref", "bad\x00ref", "bad\x7fref", "a//b", "x..y", strings.Repeat("x", 4097)} {
		if _, err := g.ParentPull(t.Context(), "owner/repo", ref); !hasCode(err, "provider_invalid_response") {
			t.Fatalf("unsafe ref accepted %q: %v", ref, err)
		}
	}
	if _, err := g.ParentPull(t.Context(), "owner/repo/other", "valid"); !hasCode(err, "provider_invalid_response") {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := g.ParentPull(ctx, "owner/repo", "feature/parent"); !hasCode(err, "provider_unavailable") {
		t.Fatal("canceled read was attempted", err)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid or canceled request reached provider", calls.Load())
	}

	started := make(chan struct{})
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
	defer blocked.Close()
	active, cancelActive := context.WithCancel(t.Context())
	defer cancelActive()
	result := make(chan error, 1)
	go func() {
		_, err := testGitHub(blocked, "fixture").ParentPull(active, "owner/repo", "feature/parent")
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancelActive()
	select {
	case err := <-result:
		if !hasCode(err, "provider_unavailable") {
			t.Fatal("cancellation did not terminate", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled parent read did not finish")
	}
}

func TestPullHeadRefOptionalLegacyCaptureAndValidation(t *testing.T) {
	raw := parentFreshFixture("feature/parent+phase")
	pullWire := func() githubPull {
		data, _ := json.Marshal(raw)
		var wire githubPull
		if err := decodeProviderJSON(data, &wire); err != nil {
			t.Fatal(err)
		}
		return wire
	}
	withRef, err := normalizePull(pullWire(), 2)
	if err != nil || withRef.HeadRef != "feature/parent+phase" {
		t.Fatal(withRef, err)
	}
	data, _ := json.Marshal(withRef)
	if _, err = ValidateHistoryFacts("pull", data); err != nil {
		t.Fatal("new retained head ref rejected", err)
	}
	delete(parentObject(raw, "head"), "ref")
	legacy, err := normalizePull(pullWire(), 2)
	if err != nil || legacy.HeadRef != "" {
		t.Fatal("missing legacy ref rejected", legacy, err)
	}
	old, _ := json.Marshal(legacy)
	if strings.Contains(string(old), "HeadRef") {
		t.Fatal("optional unknown ref changed old capture bytes")
	}
	if _, err = ValidateHistoryFacts("pull", old); err != nil {
		t.Fatal("old retained capture rejected", err)
	}
	var roundtrip Pull
	if json.Unmarshal(old, &roundtrip) != nil {
		t.Fatal("old capture could not decode")
	}
	again, _ := json.Marshal(roundtrip)
	if string(old) != string(again) {
		t.Fatal("old capture changed on roundtrip")
	}
	for _, ref := range []string{"", "bad\nref", "bad\x7fref", strings.Repeat("x", 4097)} {
		parentObject(raw, "head")["ref"] = ref
		if _, err := normalizePull(pullWire(), 2); err == nil {
			t.Fatalf("malformed supplied ref accepted %q", ref)
		}
		if ref != "" {
			legacy.HeadRef = ref
			bad, _ := json.Marshal(legacy)
			if _, err := ValidateHistoryFacts("pull", bad); err == nil {
				t.Fatal("malformed retained head ref accepted")
			}
		}
	}
}

func TestParentPullUnavailableAdapterDoesNotPerformIO(t *testing.T) {
	var calls atomic.Int64
	client := &http.Client{Transport: headRunTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader("[]"))}, nil
	})}
	for name, g := range map[string]*GitHub{
		"nil receiver":     nil,
		"zero adapter":     {},
		"nil client":       {baseURL: githubAPIURL},
		"invalid base URL": {baseURL: "https://foreign.invalid", httpClient: client},
		"empty base URL":   {httpClient: client},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Error("unavailable adapter panicked")
				}
			}()
			pull, err := g.ParentPull(t.Context(), "owner/repo", "feature/parent")
			if !hasCode(err, "provider_unavailable") || pull != (Pull{}) {
				t.Fatal("unavailable adapter accepted", pull, err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("unavailable adapter performed I/O", calls.Load())
	}
}
