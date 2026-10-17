package reviewprovider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const historyReviewRow = `{"id":11,"body":"source\n<script>data</script>","state":"COMMENTED","commit_id":"cccccccccccccccccccccccccccccccccccccccc","user":{"id":7,"login":"reviewer","type":"Bot"},"submitted_at":"2026-10-01T01:02:03.123456789+13:00"}`
const historyCommentRow = `{"id":12,"pull_request_review_id":11,"in_reply_to_id":10,"body":"reply\nsource","user":{"id":7,"login":"reviewer","type":"Bot"},"path":"internal/a.go","line":5,"start_line":4,"original_line":7,"original_start_line":6,"side":"RIGHT","start_side":"RIGHT","commit_id":"cccccccccccccccccccccccccccccccccccccccc","original_commit_id":"dddddddddddddddddddddddddddddddddddddddd","created_at":"2026-10-01T01:02:03.123456789+13:00","updated_at":"2026-10-02T01:02:03+13:00"}`

type historyPageCase struct {
	name, path, row string
	read            func(context.Context, *GitHub, string, int64, int64) (any, int64, error)
	all             func(context.Context, *GitHub) (any, error)
}

func historyPageCases() []historyPageCase {
	return []historyPageCase{
		{"reviews", "/repos/owner/repo/pulls/2/reviews", historyReviewRow, func(ctx context.Context, g *GitHub, r string, n, p int64) (any, int64, error) {
			v, e := g.ReviewsPage(ctx, r, n, p)
			return v.Items, v.NextPage, e
		}, func(ctx context.Context, g *GitHub) (any, error) { return g.Reviews(ctx, "owner/repo", 2) }},
		{"review-comments", "/repos/owner/repo/pulls/2/comments", historyCommentRow, func(ctx context.Context, g *GitHub, r string, n, p int64) (any, int64, error) {
			v, e := g.ReviewCommentsPage(ctx, r, n, p)
			return v.Items, v.NextPage, e
		}, func(ctx context.Context, g *GitHub) (any, error) { return g.ReviewComments(ctx, "owner/repo", 2) }},
		{"issue-comments", "/repos/owner/repo/issues/2/comments", commentJSON("source\n<script>data</script>"), func(ctx context.Context, g *GitHub, r string, n, p int64) (any, int64, error) {
			v, e := g.IssueCommentsPage(ctx, r, n, p)
			return v.Items, v.NextPage, e
		}, func(ctx context.Context, g *GitHub) (any, error) { return g.IssueComments(ctx, "owner/repo", 2) }},
	}
}

func TestHistoryPagesOneGETAndSourceConservation(t *testing.T) {
	for _, tc := range historyPageCases() {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.Path != tc.path || r.URL.Query().Encode() != "page=1&per_page=100" || r.Header.Get("Authorization") != "Bearer private-test-token" {
					t.Error("wrong selected resource or headers")
				}
				fmt.Fprint(w, "["+tc.row+"]")
			}))
			defer server.Close()
			g := testGitHub(server, "private-test-token")
			got, next, err := tc.read(context.Background(), g, "owner/repo", 2, 1)
			if err != nil || next != 0 || calls.Load() != 1 {
				t.Fatalf("single page: next=%d calls=%d error=%v", next, calls.Load(), err)
			}
			full, err := tc.all(context.Background(), g)
			if err != nil || !reflect.DeepEqual(got, full) {
				t.Fatalf("existing normalized source fields changed: %v", err)
			}
			switch rows := got.(type) {
			case []HostedReview:
				if rows[0].ID != 11 || rows[0].Author.ID != 7 || rows[0].Author.Kind != "Bot" || rows[0].SubmittedAt.Nanosecond() != 123456789 {
					t.Fatal("review source lost")
				}
			case []ReviewComment:
				if rows[0].InReplyToID != 10 || rows[0].ReviewID != 11 || *rows[0].OriginalStartLine != 6 || rows[0].CreatedAt.Nanosecond() != 123456789 {
					t.Fatal("reply source lost")
				}
			}
		})
	}
}

func TestHistoryPagesAdjacentContinuationAndEmptyTerminal(t *testing.T) {
	for _, tc := range historyPageCases() {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			var base string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != tc.path || r.URL.Query().Get("per_page") != "100" {
					t.Error("wrong resource")
				}
				switch r.URL.Query().Get("page") {
				case "100":
					alias := strings.Replace(tc.path, "/repos/owner/repo/", "/repositories/123/", 1)
					w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=101&per_page=100>; rel="next"`, base, alias))
					fmt.Fprint(w, "["+tc.row+"]")
				case "101", "2147483647":
					fmt.Fprint(w, "[]")
				default:
					t.Error("unexpected automatic page")
					fmt.Fprint(w, "[]")
				}
			}))
			defer server.Close()
			base = server.URL
			g := testGitHub(server, "")
			_, next, err := tc.read(context.Background(), g, "owner/repo", 2, 100)
			if err != nil || next != 101 || calls.Load() != 1 {
				t.Fatalf("adjacent page: next=%d calls=%d err=%v", next, calls.Load(), err)
			}
			for _, page := range []int64{101, 2147483647} {
				rows, next, err := tc.read(context.Background(), g, "owner/repo", 2, page)
				if err != nil || next != 0 || reflect.ValueOf(rows).IsNil() || reflect.ValueOf(rows).Len() != 0 {
					t.Fatalf("empty terminal: %v", err)
				}
			}
			if calls.Load() != 3 {
				t.Fatal("automatic drain")
			}
		})
	}
}

func TestHistoryPagesRejectUntrustedContinuation(t *testing.T) {
	links := []string{
		`https://foreign.invalid/PATH?page=2&per_page=100`,
		`BASE/PATH?page=3&per_page=100`,
		`BASE/PATH?page=2&per_page=100&head_sha=reflected`,
		`BASE/PATH?page=2&per_page=100&page=2`,
		`BASE/PATH?page=02&per_page=100`,
		`BASE/PATH?page=2&per_page=99`,
		`BASE/repos/owner/other/pulls/2/reviews?page=2&per_page=100`,
	}
	for _, tc := range historyPageCases() {
		for i, link := range links {
			t.Run(tc.name+strconv.Itoa(i), func(t *testing.T) {
				var calls atomic.Int64
				var base string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					target := strings.ReplaceAll(strings.ReplaceAll(link, "BASE", base), "/PATH", tc.path)
					w.Header().Set("Link", "<"+target+">; rel=\"next\"")
					fmt.Fprint(w, "["+tc.row+"]")
				}))
				defer server.Close()
				base = server.URL
				rows, next, err := tc.read(context.Background(), testGitHub(server, ""), "owner/repo", 2, 1)
				if !hasCode(err, "provider_invalid_response") || next != 0 || !reflect.ValueOf(rows).IsNil() || calls.Load() != 1 {
					t.Fatal("untrusted continuation accepted or followed")
				}
			})
		}
	}
}

func TestHistoryPagesRejectMalformedRowsAndBounds(t *testing.T) {
	for _, tc := range historyPageCases() {
		bodies := map[string]string{
			"duplicate-id":       "[" + tc.row + "," + tc.row + "]",
			"invalid-actor":      "[" + strings.Replace(tc.row, `"id":7`, `"id":0`, 1) + "]",
			"null":               "null",
			"object":             "{}",
			"null-row":           "[null]",
			"duplicate-key":      "[" + strings.Replace(tc.row, `"id":`, `"id":99,"id":`, 1) + "]",
			"oversized-text":     "[" + strings.Replace(tc.row, `"body":`, `"body":"`+strings.Repeat("x", githubMaxText+1)+`","ignored":`, 1) + "]",
			"too-many":           "[" + strings.TrimSuffix(strings.Repeat(tc.row+",", 101), ",") + "]",
			"oversized-response": strings.Repeat(" ", githubMaxBody+1),
		}
		if tc.name == "issue-comments" {
			bodies["invalid-actor"] = "[" + strings.Replace(tc.row, `"id":8`, `"id":0`, 1) + "]"
		}
		for name, body := range bodies {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				var calls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, body) }))
				defer server.Close()
				rows, next, err := tc.read(context.Background(), testGitHub(server, ""), "owner/repo", 2, 1)
				if !hasCode(err, "provider_invalid_response") || next != 0 || !reflect.ValueOf(rows).IsNil() || calls.Load() != 1 {
					t.Fatalf("malformed page accepted: next=%d calls=%d err=%v", next, calls.Load(), err)
				}
			})
		}
	}
}

func TestHistoryPagesValidateBeforeDispatchAndCancel(t *testing.T) {
	for _, tc := range historyPageCases() {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, "[]") }))
			defer server.Close()
			g := testGitHub(server, "")
			for _, p := range []int64{-1, 0, 2147483648} {
				if _, _, err := tc.read(context.Background(), g, "owner/repo", 2, p); err == nil {
					t.Fatal("invalid page accepted")
				}
			}
			for _, r := range []string{"owner/repo?x=1", "owner/repo/extra"} {
				if _, _, err := tc.read(context.Background(), g, r, 2, 1); err == nil {
					t.Fatal("invalid repo accepted")
				}
			}
			if _, _, err := tc.read(context.Background(), g, "owner/repo", 0, 1); err == nil {
				t.Fatal("invalid PR accepted")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, _, err := tc.read(ctx, g, "owner/repo", 2, 1); !hasCode(err, "provider_unavailable") {
				t.Fatal("canceled request accepted")
			}
			if calls.Load() != 0 {
				t.Fatal("preflight dispatched HTTP")
			}
			if _, _, err := tc.read(context.Background(), nil, "owner/repo", 2, 1); !hasCode(err, "provider_unavailable") {
				t.Fatal("nil adapter accepted")
			}
		})
	}
}

func TestHistoryPagesCancellationStopsInFlightGET(t *testing.T) {
	entered := make(chan struct{})
	ended := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(ended) }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := testGitHub(server, "").ReviewsPage(ctx, "owner/repo", 2, 1); result <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("GET not dispatched")
	}
	cancel()
	select {
	case err := <-result:
		if !hasCode(err, "provider_unavailable") {
			t.Fatal("wrong cancellation result")
		}
	case <-time.After(time.Second):
		t.Fatal("GET did not cancel")
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("HTTP handler retained")
	}
}

func TestHistoryPagesUpperBoundAndRateLimitNoRetry(t *testing.T) {
	for _, tc := range historyPageCases() {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			var base string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Query().Get("page") == "2147483647" {
					w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=2147483648&per_page=100>; rel="next"`, base, tc.path))
					fmt.Fprint(w, "[]")
					return
				}
				w.Header().Set("Retry-After", "12")
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprint(w, "private provider diagnostics")
			}))
			defer server.Close()
			base = server.URL
			g := testGitHub(server, "")
			if _, _, err := tc.read(context.Background(), g, "owner/repo", 2, 2147483647); !hasCode(err, "provider_invalid_response") {
				t.Fatal("overflow next page accepted")
			}
			_, _, err := tc.read(context.Background(), g, "owner/repo", 2, 1)
			e, ok := err.(*Error)
			if !ok || e.Code != "provider_rate_limited" || e.Status != 429 || e.RetryAfterSeconds != 12 || e.OutcomeUnknown || strings.Contains(e.Message, "private") || calls.Load() != 2 {
				t.Fatal("rate limit not retained safely or request retried")
			}
		})
	}
}
