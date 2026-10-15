package reviewprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func observationIssue(id int64, body string) map[string]any {
	return map[string]any{"id": id, "body": body, "user": map[string]any{"id": 7, "login": "posting-account", "type": "User"}, "created_at": "2026-10-01T00:00:00Z", "updated_at": "2026-10-01T00:00:00Z"}
}

func observationReview(id int64, body string) map[string]any {
	return map[string]any{"id": id, "body": body, "user": map[string]any{"id": 8, "login": "provider-account", "type": "Bot"}, "state": "COMMENTED", "commit_id": strings.Repeat("a", 40), "submitted_at": "2026-10-01T00:00:01Z"}
}

func observationInline(id, replyTo int64, body string) map[string]any {
	value := map[string]any{"id": id, "pull_request_review_id": 21, "body": body, "user": map[string]any{"id": 8, "login": "provider-account", "type": "Bot"}, "path": "internal/synthetic.go", "line": 3, "commit_id": strings.Repeat("a", 40), "original_commit_id": strings.Repeat("a", 40), "created_at": "2026-10-01T00:00:00Z", "updated_at": "2026-10-01T00:00:00Z"}
	if replyTo != 0 {
		value["in_reply_to_id"] = replyTo
	}
	return value
}

func TestObserveHostedSelectedCompletePaginatedCollectionsPreserveBodiesAndReplies(t *testing.T) {
	var calls []string
	body := "Provider text <script>plain</script> & \"quoted\"\n<!-- synthetic marker -->"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer synthetic-secret" || r.URL.Query().Get("per_page") != "100" || len(r.URL.Query()) != 2 {
			t.Errorf("unexpected method, binding or pagination")
		}
		page := r.URL.Query().Get("page")
		calls = append(calls, r.URL.Path+"?page="+page)
		if page == "1" {
			w.Header().Set("Link", "<"+serverURL(r)+r.URL.Path+"?page=2&per_page=100>; rel=next")
		}
		var value any
		switch r.URL.Path {
		case "/repos/owner/repo/issues/42/comments":
			id := int64(11)
			if page == "2" {
				id++
			}
			value = observationIssue(id, body)
		case "/repos/owner/repo/pulls/42/reviews":
			id := int64(21)
			if page == "2" {
				id++
			}
			value = observationReview(id, body)
		case "/repos/owner/repo/pulls/42/comments":
			id, reply := int64(31), int64(0)
			if page == "2" {
				id++
				reply = 31
			}
			value = observationInline(id, reply, body)
		default:
			t.Errorf("cross-target request: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]any{value})
	}))
	defer server.Close()
	before := time.Now().UTC()
	snapshot, err := testGitHub(server, "synthetic-secret").ObserveHosted(t.Context(), "owner/repo", 42)
	after := time.Now().UTC()
	if err != nil || !snapshot.Complete || snapshot.ObservedAt.Location() != time.UTC || snapshot.ObservedAt.Before(before) || snapshot.ObservedAt.After(after) {
		t.Fatalf("not a complete freshly timed snapshot: %+v %v", snapshot, err)
	}
	want := []string{"/repos/owner/repo/pulls/42/reviews?page=1", "/repos/owner/repo/pulls/42/reviews?page=2", "/repos/owner/repo/pulls/42/comments?page=1", "/repos/owner/repo/pulls/42/comments?page=2", "/repos/owner/repo/issues/42/comments?page=1", "/repos/owner/repo/issues/42/comments?page=2"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("selected sequential pages changed: %v", calls)
	}
	if len(snapshot.IssueComments) != 2 || len(snapshot.Reviews) != 2 || len(snapshot.ReviewComments) != 2 {
		t.Fatal("collection incomplete")
	}
	if snapshot.IssueComments[0].Body != body || snapshot.Reviews[0].Body != body || snapshot.ReviewComments[1].Body != body {
		t.Fatal("source prose was transformed")
	}
	root, reply := snapshot.ReviewComments[0], snapshot.ReviewComments[1]
	if root.InReplyToID != 0 || reply.InReplyToID != root.ID || root.ReviewID != 21 || reply.ReviewID != 21 || root.Path != "internal/synthetic.go" || root.Line == nil || *root.Line != 3 || root.Author.ID != 8 || root.Author.Kind != "Bot" || root.OriginalHeadSHA != strings.Repeat("a", 40) {
		t.Fatal("root/reply metadata lost")
	}
}

func TestObserveHostedPartialFailurePreservesOnlyCompletedCollections(t *testing.T) {
	paths := []string{"/repos/owner/repo/pulls/42/reviews", "/repos/owner/repo/pulls/42/comments", "/repos/owner/repo/issues/42/comments"}
	for failAt := 0; failAt < 3; failAt++ {
		t.Run(strconv.Itoa(failAt), func(t *testing.T) {
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || calls >= len(paths) || r.URL.Path != paths[calls] {
					t.Errorf("unexpected retry/selection: %s", r.URL.Path)
				}
				current := calls
				calls++
				if current == failAt {
					w.WriteHeader(503)
					_, _ = w.Write([]byte("private provider failure body"))
					return
				}
				var value any
				switch current {
				case 0:
					value = observationReview(21, "review")
				case 1:
					value = observationInline(31, 0, "inline")
				default:
					value = observationIssue(11, "issue")
				}
				_ = json.NewEncoder(w).Encode([]any{value})
			}))
			defer server.Close()
			snapshot, err := testGitHub(server, "synthetic-secret").ObserveHosted(t.Context(), "owner/repo", 42)
			if !hasCode(err, "provider_unavailable") || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "synthetic-secret") || snapshot.Complete || !snapshot.ObservedAt.IsZero() || calls != failAt+1 {
				t.Fatalf("unsafe failure/completeness: %+v %v calls%d", snapshot, err, calls)
			}
			if (snapshot.Reviews != nil) != (failAt > 0) || (snapshot.ReviewComments != nil) != (failAt > 1) || snapshot.IssueComments != nil {
				t.Fatalf("missing collection fabricated or prior collection lost: %+v", snapshot)
			}
		})
	}
}

func TestObserveHostedFailureOnLaterPageDoesNotInventPartialCollection(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/repos/owner/repo/pulls/42/reviews" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/repos/owner/repo/pulls/42/comments" {
			t.Error("unexpected target/method")
		}
		if r.URL.Query().Get("page") == "2" {
			w.WriteHeader(429)
			return
		}
		w.Header().Set("Link", "<"+serverURL(r)+r.URL.Path+"?page=2&per_page=100>; rel=next")
		_ = json.NewEncoder(w).Encode([]any{observationInline(31, 0, "first incomplete page")})
	}))
	defer server.Close()
	snapshot, err := testGitHub(server, "").ObserveHosted(t.Context(), "owner/repo", 42)
	if !hasCode(err, "provider_rate_limited") || snapshot.Complete || snapshot.Reviews == nil || snapshot.ReviewComments != nil || snapshot.IssueComments != nil || calls.Load() != 3 {
		t.Fatalf("partial evidence escaped: %+v %v", snapshot, err)
	}
}

func TestObserveHostedEmptySuccessAndInvalidSelectorsNoHTTP(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet {
			t.Error("unexpected mutation")
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()
	g := testGitHub(server, "")
	for _, input := range []struct {
		repo   string
		number int64
	}{{"other/repo/escape", 42}, {"owner/repo?query", 42}, {"owner/repo", 0}, {"owner/repo", 2147483648}} {
		snapshot, err := g.ObserveHosted(t.Context(), input.repo, input.number)
		if err == nil || snapshot.Complete || calls.Load() != 0 {
			t.Fatal("invalid selector reached HTTP")
		}
	}
	for _, ctx := range []context.Context{nil, func() context.Context { c, cancel := context.WithCancel(t.Context()); cancel(); return c }()} {
		if _, err := g.ObserveHosted(ctx, "owner/repo", 42); !hasCode(err, "provider_unavailable") || calls.Load() != 0 {
			t.Fatal("absent/canceled parent reached HTTP")
		}
	}
	snapshot, err := g.ObserveHosted(t.Context(), "owner/repo", 42)
	if err != nil || !snapshot.Complete || snapshot.IssueComments == nil || snapshot.Reviews == nil || snapshot.ReviewComments == nil || calls.Load() != 3 {
		t.Fatalf("empty full collection not distinguished: %+v %v", snapshot, err)
	}
}

func TestObserveHostedCancellationReleasesHTTPAndStopsLaterCollections(t *testing.T) {
	entered := make(chan struct{})
	released := make(chan struct{})
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/repos/owner/repo/pulls/42/reviews" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		if r.URL.Path != "/repos/owner/repo/pulls/42/comments" {
			t.Error("unexpected third collection")
		}
		close(entered)
		<-r.Context().Done()
		close(released)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		snapshot HostedPublicationSnapshot
		err      error
	}
	done := make(chan result, 1)
	go func() { s, e := testGitHub(server, "").ObserveHosted(ctx, "owner/repo", 42); done <- result{s, e} }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case got := <-done:
		if !hasCode(got.err, "provider_unavailable") || got.snapshot.Complete || got.snapshot.Reviews == nil || got.snapshot.ReviewComments != nil || got.snapshot.IssueComments != nil {
			t.Fatalf("cancellation misreported %+v %v", got.snapshot, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled observer remained blocked")
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("provider HTTP request not canceled")
	}
	if calls.Load() != 2 {
		t.Fatal("cancellation retried or started third collection")
	}
}

type observationTransport func(*http.Request) (*http.Response, error)

func (f observationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestObserveHostedOneSharedDeadlineAcrossCollections(t *testing.T) {
	for _, shortParent := range []bool{false, true} {
		t.Run(fmt.Sprint(shortParent), func(t *testing.T) {
			var deadline time.Time
			calls := 0
			ctx := t.Context()
			cancel := func() {}
			if shortParent {
				ctx, cancel = context.WithTimeout(ctx, time.Second)
			}
			defer cancel()
			g, _ := NewGitHub("")
			g.httpClient = &http.Client{Transport: observationTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				got, ok := r.Context().Deadline()
				if !ok {
					t.Fatal("missing aggregate budget")
				}
				if calls == 1 {
					deadline = got
					remaining := time.Until(got)
					if remaining <= 0 || remaining > hostedObservationLimit || shortParent && remaining > time.Second {
						t.Fatal("parent/aggregate deadline not respected")
					}
				} else if !got.Equal(deadline) {
					t.Error("new independent collection deadline")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("[]"))}, nil
			})}
			snapshot, err := g.ObserveHosted(ctx, "owner/repo", 42)
			if err != nil || !snapshot.Complete || calls != 3 {
				t.Fatal("collections did not share one deadline", err)
			}
		})
	}
}

func TestObserveHostedSharedAggregateBudgetRetainsEarlierCollections(t *testing.T) {
	body := strings.Repeat("x", githubMaxText)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Query().Get("page") != "1" {
			t.Error("unexpected mutation/retry")
		}
		values := make([]any, 100)
		for i := range values {
			switch r.URL.Path {
			case "/repos/owner/repo/issues/42/comments":
				values[i] = observationIssue(int64(i+1), body)
			case "/repos/owner/repo/pulls/42/reviews":
				values[i] = observationReview(int64(i+1), body)
			case "/repos/owner/repo/pulls/42/comments":
				values[i] = observationInline(int64(i+1), 0, body)
			default:
				t.Error("unexpected target")
				http.NotFound(w, r)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(values)
	}))
	defer server.Close()
	snapshot, err := testGitHub(server, "").ObserveHosted(t.Context(), "owner/repo", 42)
	if !hasCode(err, "provider_invalid_response") || snapshot.Complete || !snapshot.ObservedAt.IsZero() || calls != 3 || len(snapshot.Reviews) != 100 || len(snapshot.ReviewComments) != 100 || snapshot.IssueComments != nil {
		t.Fatalf("aggregate overflow retained/invented complete evidence: issues%d reviews%d inline%d complete%t calls%d err%v", len(snapshot.IssueComments), len(snapshot.Reviews), len(snapshot.ReviewComments), snapshot.Complete, calls, err)
	}
}

func TestObserveHostedSharedShapeValidationRejectsDuplicateEvidence(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/repos/owner/repo/pulls/42/reviews":
			_ = json.NewEncoder(w).Encode([]any{observationReview(1, "retained")})
		case "/repos/owner/repo/pulls/42/comments":
			_ = json.NewEncoder(w).Encode([]any{observationInline(2, 0, "first"), observationInline(2, 0, "conflicting")})
		default:
			t.Error("should stop before third collection")
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	snapshot, err := testGitHub(server, "").ObserveHosted(t.Context(), "owner/repo", 42)
	if !hasCode(err, "provider_invalid_response") || snapshot.Complete || calls != 2 || len(snapshot.Reviews) != 1 || snapshot.ReviewComments != nil || snapshot.IssueComments != nil {
		t.Fatal("duplicate snapshot evidence accepted or prior observation lost", err)
	}
}

func TestObserveHostedLastCommandCollectionClosesInterleavedPublicationWindow(t *testing.T) {
	s, intent, poster, bot, earliest := publicationFixture(t)
	at := time.Now().UTC().Add(-10 * time.Second).Truncate(time.Second)
	earliest = at.Add(-time.Second)
	trigger := s.IssueComments[0]
	trigger.CreatedAt, trigger.UpdatedAt = at, at
	review := s.Reviews[0]
	review.SubmittedAt = at.Add(4 * time.Second)
	root := s.ReviewComments[0]
	root.CreatedAt, root.UpdatedAt = at.Add(3*time.Second), at.Add(3*time.Second)
	competing := trigger
	competing.ID++
	competing.Body = "@coderabbitai full review"
	competing.Author = Author{ID: 777, Login: "other-requester", Kind: "User"}
	competing.CreatedAt, competing.UpdatedAt = at.Add(2*time.Second), at.Add(2*time.Second)
	wireActor := func(a Author) map[string]any { return map[string]any{"id": a.ID, "login": a.Login, "type": a.Kind} }
	wireIssue := func(c IssueComment) map[string]any {
		return map[string]any{"id": c.ID, "body": c.Body, "user": wireActor(c.Author), "created_at": c.CreatedAt.Format(time.RFC3339Nano), "updated_at": c.UpdatedAt.Format(time.RFC3339Nano)}
	}
	var paths []string
	commandVisible := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Method != http.MethodGet {
			t.Error("unexpected mutation")
		}
		switch r.URL.Path {
		case "/repos/owner/repo/pulls/42/reviews":
			// Model a competing command becoming visible between independent reads.
			commandVisible = true
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"id": review.ID, "body": review.Body, "user": wireActor(bot), "state": review.State, "commit_id": review.HeadSHA, "submitted_at": review.SubmittedAt.Format(time.RFC3339Nano)}})
		case "/repos/owner/repo/pulls/42/comments":
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"id": root.ID, "pull_request_review_id": root.ReviewID, "body": root.Body, "user": wireActor(bot), "path": root.Path, "line": root.Line, "commit_id": root.HeadSHA, "original_commit_id": root.OriginalHeadSHA, "created_at": root.CreatedAt.Format(time.RFC3339Nano), "updated_at": root.UpdatedAt.Format(time.RFC3339Nano)}})
		case "/repos/owner/repo/issues/42/comments":
			values := []any{wireIssue(trigger)}
			if commandVisible {
				values = append(values, wireIssue(competing))
			}
			_ = json.NewEncoder(w).Encode(values)
		default:
			t.Error("unexpected selected resource")
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	snapshot, err := testGitHub(server, "").ObserveHosted(t.Context(), "owner/repo", 42)
	if err != nil || !snapshot.Complete || len(snapshot.IssueComments) != 2 {
		t.Fatalf("interleaved command not included: %+v %v", snapshot, err)
	}
	wantPaths := []string{"/repos/owner/repo/pulls/42/reviews", "/repos/owner/repo/pulls/42/comments", "/repos/owner/repo/issues/42/comments"}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("command collection must follow publication reads: %v", paths)
	}
	got, err := ResolveHostedPublication(snapshot, intent, poster, bot, earliest)
	if err != nil || got.State != "uncertain" || got.Reason != "attribution_window_closed" || len(got.EligibleReviews) != 0 {
		t.Fatalf("competing command's review attributed to old trigger: %+v %v", got, err)
	}
	// Establish that the same valid publication would otherwise be attributable;
	// the competing command, rather than malformed synthetic evidence, stops it.
	snapshot.IssueComments = snapshot.IssueComments[:1]
	got, err = ResolveHostedPublication(snapshot, intent, poster, bot, earliest)
	if err != nil || got.State != "attributed" {
		t.Fatalf("invalid baseline obscures interleaving regression: %+v %v", got, err)
	}
}
