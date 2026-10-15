package reviewprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func testGitHub(server *httptest.Server, token string) *GitHub {
	client := server.Client()
	client.Timeout = githubRequestLimit
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &GitHub{token: token, baseURL: server.URL, httpClient: client}
}

func hasCode(err error, code string) bool {
	var providerErr *Error
	return errors.As(err, &providerErr) && providerErr.Code == code
}

func TestGitHubReadsValidateEndpointShapesAndPreserveText(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-secret" || r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" || r.Header.Get("User-Agent") != "FireController" {
			t.Errorf("missing expected provider headers")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"id":7,"login":"reviewer","type":"User","future":{"ignored":true}}`))
		case "/repositories/1418705294":
			_, _ = w.Write([]byte(`{"id":1418705294,"full_name":"benhook1013/FireController","default_branch":"main","future":null}`))
		case "/repos/benhook1013/FireController/pulls/2":
			_, _ = w.Write([]byte(`{"title":"Review \u0026 preserve <source>","number":2,"state":"closed","merged":true,"head":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repo":null},"base":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","ref":"main","repo":{"id":1418705294,"full_name":"benhook1013/FireController","default_branch":"main"}},"changed_files":4}`))
		case "/repos/benhook1013/FireController/issues/2/comments":
			if r.Method == http.MethodPost {
				var body struct {
					Body string `json:"body"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.Body != "first line\n<script>plain</script>" {
					t.Errorf("comment body was not preserved")
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(commentJSON("first line\n<script>plain</script>")))
				return
			}
			_, _ = w.Write([]byte("[" + commentJSON("line one\nline two") + "]"))
		case "/repos/benhook1013/FireController/pulls/2/reviews":
			_, _ = w.Write([]byte(`[{"id":11,"body":"pending","state":"pending","commit_id":"cccccccccccccccccccccccccccccccccccccccc","user":{"id":7,"login":"reviewer","type":"User"},"submitted_at":null}]`))
		case "/repos/benhook1013/FireController/pulls/2/comments":
			_, _ = w.Write([]byte(`[{"id":12,"pull_request_review_id":11,"body":"inline","user":{"id":7,"login":"reviewer","type":"User"},"path":"internal/a.go","line":null,"commit_id":"cccccccccccccccccccccccccccccccccccccccc","original_commit_id":"dddddddddddddddddddddddddddddddddddddddd","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}]`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	g := testGitHub(server, "test-secret")

	author, err := g.CurrentActor(context.Background())
	if err != nil || author.ID != 7 || author.Login != "reviewer" || author.Kind != "User" {
		t.Fatalf("CurrentActor() = %#v, %v", author, err)
	}
	repository, err := g.Repository(context.Background(), 1418705294)
	if err != nil || repository.FullName != "benhook1013/FireController" || repository.DefaultBranch != "main" {
		t.Fatalf("Repository() = %#v, %v", repository, err)
	}
	pull, err := g.Pull(context.Background(), "benhook1013/FireController", 2)
	if err != nil || !pull.Merged || pull.Title != "Review & preserve <source>" || pull.RepositoryID != 1418705294 || pull.HeadSHA[0] != 'a' {
		t.Fatalf("Pull() = %#v, %v", pull, err)
	}
	comments, err := g.IssueComments(context.Background(), "benhook1013/FireController", 2)
	if err != nil || len(comments) != 1 || comments[0].Body != "line one\nline two" {
		t.Fatalf("IssueComments() = %#v, %v", comments, err)
	}
	reviews, err := g.Reviews(context.Background(), "benhook1013/FireController", 2)
	if err != nil || len(reviews) != 1 || reviews[0].SubmittedAt != (time.Time{}) || reviews[0].State != "pending" {
		t.Fatalf("Reviews() = %#v, %v", reviews, err)
	}
	reviewComments, err := g.ReviewComments(context.Background(), "benhook1013/FireController", 2)
	if err != nil || len(reviewComments) != 1 || reviewComments[0].Line != nil {
		t.Fatalf("ReviewComments() = %#v, %v", reviewComments, err)
	}
	posted, err := g.PostIssueComment(context.Background(), "benhook1013/FireController", 2, "first line\n<script>plain</script>")
	if err != nil || posted.Body != "first line\n<script>plain</script>" || calls.Load() != 7 {
		t.Fatalf("PostIssueComment() = %#v, %v; calls=%d", posted, err, calls.Load())
	}
}

func commentJSON(body string) string {
	encoded, _ := json.Marshal(body)
	return fmt.Sprintf(`{"id":21,"body":%s,"user":{"id":8,"login":"author","type":"User"},"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}`, encoded)
}

func TestGitHubPaginationValidatesNextAndNeverFollowsItDirectly(t *testing.T) {
	var requests atomic.Int64
	var foreignRequests atomic.Int64
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignRequests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("[]"))
	}))
	defer foreign.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer secret" || r.URL.Path != "/repos/owner/repo/issues/4/comments" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", "<"+foreign.URL+"/repos/owner/repo/issues/4/comments?page=2&per_page=100>; rel=\"next\"")
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()
	g := testGitHub(server, "secret")
	_, err := g.IssueComments(context.Background(), "owner/repo", 4)
	if !hasCode(err, "provider_invalid_response") || requests.Load() != 1 || foreignRequests.Load() != 0 {
		t.Fatalf("untrusted next was not rejected before following: err=%v requests=%d foreign=%d", err, requests.Load(), foreignRequests.Load())
	}
}

func TestGitHubPaginationFollowsOnlyConstructedSequentialPages(t *testing.T) {
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("credential missing from trusted request")
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		w.Header().Set("Content-Type", "application/json")
		if page == "1" {
			w.Header().Set("Link", "<"+serverURL(r)+"/repos/owner/repo/issues/4/comments?page=2&per_page=100>; rel=\"next\"")
			_, _ = w.Write([]byte("[]"))
			return
		}
		_, _ = w.Write([]byte("[" + commentJSON("second page") + "]"))
	}))
	defer server.Close()
	// The handler cannot refer to server before construction, so point next at a
	// path-relative URL and exercise validation against the same loopback origin.
	g := testGitHub(server, "secret")
	items, err := g.IssueComments(context.Background(), "owner/repo", 4)
	if err != nil || len(items) != 1 || items[0].Body != "second page" || strings.Join(pages, ",") != "1,2" {
		t.Fatalf("page result=%#v err=%v pages=%v", items, err, pages)
	}
}

func serverURL(r *http.Request) string { return "http://" + r.Host }

func TestGitHubRejectsBadProviderShapesAndDuplicateKeys(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"duplicate known key", `{"id":1,"id":2,"login":"x","type":"User"}`},
		{"known field wrong type", `{"id":"1","login":"x","type":"User"}`},
		{"known field null", `{"id":null,"login":"x","type":"User"}`},
		{"invalid utf8", "{\"id\":1,\"login\":\"\xff\",\"type\":\"User\"}"},
		{"missing semantic field", `{"id":1,"login":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = ioWriteString(w, tc.body) }))
			defer server.Close()
			_, err := testGitHub(server, "").CurrentActor(context.Background())
			if !hasCode(err, "provider_invalid_response") {
				t.Fatalf("error=%v", err)
			}
		})
	}
	unknownServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = ioWriteString(w, `{"id":1,"login":"x","type":"User","future":{"new":true}}`)
	}))
	defer unknownServer.Close()
	if _, err := testGitHub(unknownServer, "").CurrentActor(context.Background()); err != nil {
		t.Fatalf("unknown future field should be ignored: %v", err)
	}
}

func ioWriteString(w http.ResponseWriter, value string) (int, error) { return w.Write([]byte(value)) }

func TestGitHubSelectorsAndTokenAreValidatedBeforeDispatch(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()
	for _, token := range []string{"bad token", "bad\r\nAuthorization: yes", strings.Repeat("x", 4097)} {
		if _, err := NewGitHub(token); err == nil {
			t.Fatalf("NewGitHub accepted unsafe token")
		}
	}
	g := testGitHub(server, "")
	for _, repository := range []string{"", "owner", "/repo", "owner/", "owner/..", "owner/repo?x=1", "owner/repo%2fother", "a/b/c", "owner/with space"} {
		if _, err := g.Pull(context.Background(), repository, 1); err == nil {
			t.Errorf("accepted repository %q", repository)
		}
	}
	for _, number := range []int64{0, -1, 2147483648} {
		if _, err := g.Pull(context.Background(), "owner/repo", number); err == nil {
			t.Errorf("accepted PR number %d", number)
		}
	}
	if _, err := g.Repository(context.Background(), 0); err == nil {
		t.Fatal("accepted zero repository id")
	}
	if _, err := g.PostIssueComment(context.Background(), "owner/repo", 1, "\u0000bad"); err == nil {
		t.Fatal("accepted control in comment")
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid local arguments dispatched %d requests", calls.Load())
	}
}

func TestGitHubPendingTimeAndPullIdentityAreStrict(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
	}{
		{"nonpending null time", "/repos/owner/repo/pulls/2/reviews", `[{"id":1,"body":"","state":"approved","commit_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","user":{"id":2,"login":"x","type":"User"},"submitted_at":null}]`},
		{"wrong pull identity", "/repos/owner/repo/pulls/2", `{"title":"Review \u0026 preserve <source>","number":3,"state":"open","merged":false,"head":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repo":null},"base":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","ref":"main","repo":{"id":7,"full_name":"owner/repo","default_branch":"main"}},"changed_files":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = ioWriteString(w, tc.body) }))
			defer server.Close()
			g := testGitHub(server, "")
			var err error
			if strings.HasSuffix(tc.path, "/reviews") {
				_, err = g.Reviews(context.Background(), "owner/repo", 2)
			} else {
				_, err = g.Pull(context.Background(), "owner/repo", 2)
			}
			if !hasCode(err, "provider_invalid_response") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestGitHubPullTitleRequiredAndBounded(t *testing.T) {
	base := `{"title":"Keep exact title","number":2,"state":"open","merged":false,"head":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repo":null},"base":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","ref":"main","repo":{"id":7,"full_name":"owner/repo","default_branch":"main"}},"changed_files":1}`
	for _, replacement := range []string{"", `"title":null,`, `"title":"   ",`, `"title":"bad\u0000title",`, `"title":"` + strings.Repeat("x", 501) + `",`} {
		t.Run(replacement, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = ioWriteString(w, strings.Replace(base, `"title":"Keep exact title",`, replacement, 1))
			}))
			defer server.Close()
			if _, err := testGitHub(server, "").Pull(context.Background(), "owner/repo", 2); !hasCode(err, "provider_invalid_response") {
				t.Fatalf("invalid title accepted: %v", err)
			}
		})
	}
}

func TestGitHubPostOutcomeUnknownAndNoAutomaticRetry(t *testing.T) {
	for _, mode := range []string{"malformed-success", "disconnect", "refused", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch mode {
				case "malformed-success":
					w.WriteHeader(http.StatusCreated)
					_, _ = ioWriteString(w, `{"id":"wrong"}`)
				case "disconnect":
					hijacker, ok := w.(http.Hijacker)
					if !ok {
						t.Error("test server does not support hijacking")
						return
					}
					connection, _, err := hijacker.Hijack()
					if err == nil {
						_ = connection.Close()
					}
				case "refused":
					http.Error(w, "secret provider body", http.StatusUnprocessableEntity)
				case "timeout":
					http.Error(w, "secret timeout body", http.StatusRequestTimeout)
				}
			}))
			defer server.Close()
			_, err := testGitHub(server, "secret-token").PostIssueComment(context.Background(), "owner/repo", 8, "safe body")
			if calls.Load() != 1 {
				t.Fatalf("request count=%d; automatic replay occurred", calls.Load())
			}
			var providerErr *Error
			if !errors.As(err, &providerErr) {
				t.Fatalf("error=%v", err)
			}
			wantUnknown := mode != "refused"
			if providerErr.OutcomeUnknown != wantUnknown {
				t.Fatalf("OutcomeUnknown=%v, want %v (error=%+v)", providerErr.OutcomeUnknown, wantUnknown, providerErr)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), server.URL) {
				t.Fatalf("unsafe error: %v", err)
			}
		})
	}
}

func TestGitHubRateLimitFactsAreBoundedAndSafe(t *testing.T) {
	cases := []struct {
		status  int
		headers http.Header
		code    string
		retry   int64
	}{
		{429, http.Header{"Retry-After": []string{"90000"}}, "provider_rate_limited", 86400},
		{403, http.Header{"X-Ratelimit-Remaining": []string{"0"}, "X-Ratelimit-Reset": []string{strconv.FormatInt(time.Now().Add(12*time.Second).Unix(), 10)}}, "provider_rate_limited", 12},
		{429, http.Header{"Retry-After": []string{time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)}}, "provider_rate_limited", 29},
		{403, http.Header{}, "provider_unauthorized", 0},
		{404, http.Header{}, "provider_not_found", 0},
	}
	for _, tc := range cases {
		response := &http.Response{StatusCode: tc.status, Header: tc.headers}
		err := statusError(response, false)
		var providerErr *Error
		if !errors.As(err, &providerErr) || providerErr.Code != tc.code || providerErr.RetryAfterSeconds != tc.retry || providerErr.Status != tc.status {
			t.Fatalf("statusError(%d)=%+v, want code=%s retry=%d", tc.status, providerErr, tc.code, tc.retry)
		}
	}
}

func TestGitHubCancellationIsBoundedAndDoesNotLeakProviderData(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-time.After(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"id":1,"login":"x","type":"User"}`))
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := testGitHub(server, "private-token").CurrentActor(ctx); done <- err }()
	<-started
	cancel()
	err := <-done
	if err == nil || strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("unsafe or absent cancellation error: %v", err)
	}
}

func TestNextPageRejectsAmbiguityAndPageBounds(t *testing.T) {
	base := "https://api.github.com"
	path := "/repos/o/r/issues/1/comments"
	cases := []string{
		"<https://api.github.com/repos/o/r/issues/1/comments?page=2&per_page=100>; rel=next, <https://api.github.com/repos/o/r/issues/1/comments?page=3&per_page=100>; rel=next",
		"<https://api.github.com/repos/o/r/issues/1/comments?page=1&per_page=100>; rel=next",
		"<https://api.github.com/repos/o/r/issues/1/comments?page=101&per_page=100>; rel=next",
		"<https://api.github.com/repos/o/r/issues/1/comments?page=3&per_page=100>; rel=next",
		"<https://api.github.com/repos/o/r/issues/1/comments?page=0&per_page=100>; rel=next",
		"<https://api.github.com/repos/o/r/issues/1/comments?page=-1&per_page=100>; rel=next",
		"<https://api.github.com/repos/o/r/issues/1/comments?page=9223372036854775808&per_page=100>; rel=next",
		"<https://api.github.com/repos/o/r/issues/1/comments?page=2&per_page=50>; rel=next",
		"<https://api.github.com/repos/o/r/issues/1/comments?page=2&per_page=100&access_token=x>; rel=next",
		"malformed rel=next",
		"<https://api.github.com/repos/o/r/issues/1/comments?page=02&per_page=100>; rel=next",
	}
	for _, link := range cases {
		if _, _, err := nextPage([]string{link}, base, path, 1); err == nil {
			t.Errorf("accepted invalid next link %q", link)
		}
	}
	if _, found, err := nextPage([]string{"<https://api.github.com/repos/o/r/issues/1/comments?page=2&per_page=100>; rel=next"}, base, path, 1); err != nil || !found {
		t.Fatalf("valid next page rejected: found=%v err=%v", found, err)
	}
}

func TestProviderStatusDoesNotExposeResponseBodyOrURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://evil.invalid/token")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = ioWriteString(w, "private body "+r.URL.String())
	}))
	defer server.Close()
	_, err := testGitHub(server, "super-secret").Repository(context.Background(), 3)
	if err == nil || strings.Contains(err.Error(), "private body") || strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "evil.invalid") {
		t.Fatalf("provider diagnostic leaked data: %v", err)
	}
}

func TestGitHubNewUsesFixedOriginAndRequestTimeout(t *testing.T) {
	g, err := NewGitHub("")
	if err != nil {
		t.Fatal(err)
	}
	if g.baseURL != githubAPIURL || g.httpClient.Timeout != 5*time.Second {
		t.Fatalf("unexpected production adapter configuration: base=%s timeout=%s", g.baseURL, g.httpClient.Timeout)
	}
	if validBaseURL("https://evil.invalid") || validBaseURL("https://api.github.com.evil.invalid") || validBaseURL("http://example.com") {
		t.Fatal("accepted non-test provider base")
	}
}

func TestProductionClientDoesNotFollowRedirects(t *testing.T) {
	var destinationCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/destination" {
			destinationCalls.Add(1)
			_, _ = ioWriteString(w, `{"id":1,"full_name":"owner/repo","default_branch":"main"}`)
			return
		}
		http.Redirect(w, r, "/destination", http.StatusFound)
	}))
	defer server.Close()
	g, err := NewGitHub("")
	if err != nil {
		t.Fatal(err)
	}
	g.baseURL = server.URL
	_, err = g.Repository(context.Background(), 1)
	if err == nil || destinationCalls.Load() != 0 || !hasCode(err, "provider_unavailable") {
		t.Fatalf("redirect was followed or misclassified: error=%v destination=%d", err, destinationCalls.Load())
	}
}

func TestGitHubBodyControlBounds(t *testing.T) {
	if !validBody(strings.Repeat("x", githubMaxText)) || validBody(strings.Repeat("x", githubMaxText+1)) || validBody("ok\u0001") || validBody("ok\u007f") || !validBody("tab\tline\r\n") {
		t.Fatal("body text bounds or control policy mismatch")
	}
	if !utf8.ValidString("λ") || validBody(string([]byte{0xff})) {
		t.Fatal("body UTF-8 validation failed")
	}
}

func TestGitHubStatusUnknownForWriteOnlyAtAmbiguousBoundaries(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity} {
		response := &http.Response{StatusCode: status, Header: make(http.Header)}
		err := statusError(response, true)
		var providerErr *Error
		if !errors.As(err, &providerErr) || providerErr.OutcomeUnknown {
			t.Fatalf("definitive status %d returned %v", status, err)
		}
	}
	for _, status := range []int{http.StatusAccepted, http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusTemporaryRedirect} {
		response := &http.Response{StatusCode: status, Header: make(http.Header)}
		err := statusError(response, true)
		var providerErr *Error
		if !errors.As(err, &providerErr) || !providerErr.OutcomeUnknown {
			t.Fatalf("ambiguous status %d returned %v", status, err)
		}
	}
}

func TestGitHubPaginationRejectsAggregateOverflow(t *testing.T) {
	var pageCalls atomic.Int64
	padding := strings.Repeat("x", 6<<20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := pageCalls.Add(1)
		if page < 3 {
			next := page + 1
			w.Header().Set("Link", fmt.Sprintf("<%s%s?page=%d&per_page=100>; rel=next", serverURL(r), r.URL.Path, next))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = ioWriteString(w, `[{"id":1,"body":"ok","user":{"id":2,"login":"x","type":"User"},"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z","future":"`+padding+`"}]`)
	}))
	defer server.Close()
	_, err := testGitHub(server, "").IssueComments(context.Background(), "o/r", 1)
	if !hasCode(err, "provider_invalid_response") || pageCalls.Load() != 3 {
		t.Fatalf("aggregate overflow result error=%v calls=%d", err, pageCalls.Load())
	}
}

func TestGitHubRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = ioWriteString(w, `{"id":1,"login":"x","type":"User","future":"`+strings.Repeat("x", githubMaxBody)+`"}`)
	}))
	defer server.Close()
	_, err := testGitHub(server, "").CurrentActor(context.Background())
	if !hasCode(err, "provider_invalid_response") {
		t.Fatalf("oversized response error=%v", err)
	}
}

func TestGitHubLinkQueryValuesAreConstructedNotCopied(t *testing.T) {
	query := url.Values{"page": {"2"}, "per_page": {"100"}}
	if got := query.Encode(); got != "page=2&per_page=100" {
		t.Fatalf("unexpected constructed query %q", got)
	}
}

func TestGitHubAllEmptyCollectionsAreCompleteEmptyArrays(t *testing.T) {
	for _, kind := range []string{"issues", "reviews", "review-comments"} {
		for _, twoPages := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/two-pages=%t", kind, twoPages), func(t *testing.T) {
				var calls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if twoPages && r.URL.Query().Get("page") == "1" {
						w.Header().Set("Link", "<"+serverURL(r)+r.URL.Path+"?page=2&per_page=100>; rel=next")
					}
					_, _ = ioWriteString(w, "[]")
				}))
				defer server.Close()
				g := testGitHub(server, "")
				var result any
				var err error
				switch kind {
				case "issues":
					result, err = g.IssueComments(context.Background(), "owner/repo", 1)
				case "reviews":
					result, err = g.Reviews(context.Background(), "owner/repo", 1)
				case "review-comments":
					result, err = g.ReviewComments(context.Background(), "owner/repo", 1)
				}
				encoded, _ := json.Marshal(result)
				wantCalls := int64(1)
				if twoPages {
					wantCalls = 2
				}
				if err != nil || string(encoded) != "[]" || calls.Load() != wantCalls {
					t.Fatalf("empty collection: result=%s err=%v calls=%d", encoded, err, calls.Load())
				}
			})
		}
	}
}

func TestGitHubRejectsKnownCaseAliasesAtEveryObjectDepth(t *testing.T) {
	actor := `{"id":7,"login":"reviewer","type":"User"}`
	pull := `{"title":"Review \u0026 preserve <source>","number":2,"state":"open","merged":false,"head":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repo":null},"base":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","ref":"main","repo":{"id":7,"full_name":"owner/repo","default_branch":"main"}},"changed_files":1}`
	comment := `[{"id":12,"pull_request_review_id":11,"body":"inline","user":{"id":7,"login":"reviewer","type":"User"},"path":"a.go","commit_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","original_commit_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}]`
	for _, test := range []struct{ name, endpoint, body string }{
		{"scalar overwrite", "actor", strings.Replace(actor, `"id":7`, `"id":7,"ID":8`, 1)},
		{"null alias", "actor", strings.Replace(actor, `"login":"reviewer"`, `"login":"reviewer","LOGIN":null`, 1)},
		{"malformed alias", "actor", strings.Replace(actor, `"type":"User"`, `"type":"User","TYPE":[]`, 1)},
		{"alias only", "actor", strings.Replace(actor, `"id":7`, `"ID":7`, 1)},
		{"unicode folded alias", "pull", strings.Replace(pull, `"state":"open"`, `"state":"open","ſtate":"closed"`, 1)},
		{"nested object alias", "pull", strings.Replace(pull, `"repo":null`, `"repo":null,"REPO":{"id":8,"full_name":"owner/fork","default_branch":"main"}`, 1)},
		{"nested scalar overwrite", "pull", strings.Replace(pull, `"id":7`, `"id":7,"ID":8`, 1)},
		{"nested null alias", "pull", strings.Replace(pull, `"default_branch":"main"`, `"default_branch":"main","DEFAULT_BRANCH":null`, 1)},
		{"nested malformed alias", "pull", strings.Replace(pull, `"full_name":"owner/repo"`, `"full_name":"owner/repo","FULL_NAME":{}`, 1)},
		{"array actor overwrite", "comments", strings.Replace(comment, `"id":7`, `"id":7,"ID":8`, 1)},
		{"optional alias only null", "comments", strings.Replace(comment, `"path":"a.go"`, `"path":"a.go","LINE":null`, 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = ioWriteString(w, test.body) }))
			defer server.Close()
			g := testGitHub(server, "")
			var err error
			switch test.endpoint {
			case "actor":
				_, err = g.CurrentActor(context.Background())
			case "pull":
				_, err = g.Pull(context.Background(), "owner/repo", 2)
			case "comments":
				_, err = g.ReviewComments(context.Background(), "owner/repo", 2)
			}
			if !hasCode(err, "provider_invalid_response") {
				t.Fatalf("known alias accepted: %v", err)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = ioWriteString(w, strings.Replace(actor, `"type":"User"`, `"type":"User","future":{"ID":8},"Future":null`, 1))
	}))
	defer server.Close()
	if got, err := testGitHub(server, "").CurrentActor(context.Background()); err != nil || got.ID != 7 {
		t.Fatal("unrelated future fields were rejected or changed identity")
	}
}

func TestGitHubMalformedLinkParametersNeverCompletePartialCollection(t *testing.T) {
	for _, params := range []string{
		`; rel`, `; rel=`, `; rel=""`, `; rel="   "`, `; rel=next; rel=next`, `; rel="next"; REL=prev`,
		`; rel="next`, `; rel=next"`, `; rel="next"unexpected`, `; rel=prefix"next"`, `; rel="next next"`,
		`; rel="next,prev"`, "; rel=\"next\tprev\"",
		`; rel=prev; title="unterminated`, `; rel=next; title="valid"junk`,
	} {
		t.Run(params, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Link", "<"+serverURL(r)+r.URL.Path+"?page=2&per_page=100>"+params)
				_, _ = ioWriteString(w, "["+commentJSON("first page")+"]")
			}))
			defer server.Close()
			result, err := testGitHub(server, "").IssueComments(context.Background(), "owner/repo", 1)
			if !hasCode(err, "provider_invalid_response") || result != nil || calls.Load() != 1 {
				t.Fatalf("partial result accepted or bad link followed: err=%v calls=%d", err, calls.Load())
			}
		})
	}
	path := "/repos/o/r/issues/1/comments"
	for _, params := range []string{`; rel=next`, `; rel="next"; title="comma, semicolon; and escaped \" quote"`, `; rel="NEXT"; future=value`, `; future="valid, metadata"; rel="next alternate"`} {
		link := "<https://api.github.com" + path + "?page=2&per_page=100>" + params
		if page, found, err := nextPage([]string{link}, githubAPIURL, path, 1); err != nil || !found || page != 2 {
			t.Fatalf("valid Link parameters rejected: %q page=%d found=%t err=%v", params, page, found, err)
		}
	}
}

// Numeric repository links identify the next page only. The adapter must still
// reconstruct every request with the originally selected owner/repository path.
func TestGitHubNumericRepositoryPaginationUsesOriginalCollection(t *testing.T) {
	for _, kind := range []string{"issues", "reviews", "review-comments"} {
		for _, malformedSecond := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/malformed-second=%v", kind, malformedSecond), func(t *testing.T) {
				suffix := numericPaginationSuffix(kind)
				expectedPath := "/repos/owner/repo/" + suffix
				var requests atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if r.Method != http.MethodGet || r.URL.Path != expectedPath || r.URL.RawPath != "" || r.URL.Query().Get("per_page") != "100" || r.Header.Get("Authorization") != "Bearer synthetic-pagination-token" {
						t.Errorf("unsafe reconstructed request: %s %s", r.Method, r.URL.String())
						http.NotFound(w, r)
						return
					}
					switch r.URL.Query().Get("page") {
					case "1":
						w.Header().Set("Link", "<"+serverURL(r)+"/repositories/1418705294/"+suffix+"?per_page=100&page=2>; rel=next")
						_, _ = ioWriteString(w, "["+numericPaginationItem(kind, 1)+"]")
					case "2":
						if malformedSecond {
							_, _ = ioWriteString(w, `[{"id":`)
							return
						}
						_, _ = ioWriteString(w, "["+numericPaginationItem(kind, 2)+"]")
					default:
						t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				bodies, err := readNumericPaginationCollection(t, testGitHub(server, "synthetic-pagination-token"), kind)
				if malformedSecond {
					if !hasCode(err, "provider_invalid_response") || bodies != nil {
						t.Fatal("malformed later page returned a partial complete collection", bodies, err)
					}
				} else if err != nil || strings.Join(bodies, ",") != "page one,page two" {
					t.Fatal("numeric Link rejected or source text lost", bodies, err)
				}
				if requests.Load() != 2 {
					t.Fatal("wrong request count or numeric link fetched", requests.Load())
				}
			})
		}
	}
}

func numericPaginationSuffix(kind string) string {
	switch kind {
	case "issues":
		return "issues/7/comments"
	case "reviews":
		return "pulls/7/reviews"
	default:
		return "pulls/7/comments"
	}
}

func numericPaginationItem(kind string, page int) string {
	body := "page one"
	if page == 2 {
		body = "page two"
	}
	actor := `{"id":8,"login":"synthetic-provider","type":"Bot"}`
	switch kind {
	case "issues":
		return fmt.Sprintf(`{"id":%d,"body":%q,"user":%s,"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}`, page, body, actor)
	case "reviews":
		return fmt.Sprintf(`{"id":%d,"body":%q,"state":"commented","commit_id":"%s","user":%s,"submitted_at":"2026-10-01T00:00:00Z"}`, page, body, strings.Repeat("a", 40), actor)
	default:
		return fmt.Sprintf(`{"id":%d,"pull_request_review_id":11,"body":%q,"user":%s,"path":"internal/a.go","line":null,"commit_id":"%s","original_commit_id":"%s","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}`, page, body, actor, strings.Repeat("a", 40), strings.Repeat("b", 40))
	}
}

func readNumericPaginationCollection(t *testing.T, g *GitHub, kind string) ([]string, error) {
	t.Helper()
	bodies := []string{}
	switch kind {
	case "issues":
		items, err := g.IssueComments(t.Context(), "owner/repo", 7)
		if err != nil {
			if items != nil {
				t.Error("issue-comment error returned partial items")
			}
			return nil, err
		}
		for _, item := range items {
			bodies = append(bodies, item.Body)
		}
	case "reviews":
		items, err := g.Reviews(t.Context(), "owner/repo", 7)
		if err != nil {
			if items != nil {
				t.Error("review error returned partial items")
			}
			return nil, err
		}
		for _, item := range items {
			bodies = append(bodies, item.Body)
		}
	default:
		items, err := g.ReviewComments(t.Context(), "owner/repo", 7)
		if err != nil {
			if items != nil {
				t.Error("review-comment error returned partial items")
			}
			return nil, err
		}
		for _, item := range items {
			bodies = append(bodies, item.Body)
		}
	}
	return bodies, nil
}

func TestNextPageNumericRepositoryLinkScopeAndCanonicalIDs(t *testing.T) {
	for _, base := range []string{"https://api.github.com", "https://github.example/api/v3", "https://github.example/api/v3/"} {
		for _, kind := range []string{"issues", "reviews", "review-comments"} {
			suffix := numericPaginationSuffix(kind)
			path := "/repos/owner/repo/" + suffix
			origin := strings.TrimRight(base, "/")
			linkPath := "/repositories/1418705294/" + suffix
			accepted := []string{origin + linkPath, origin + "/repositories/1/" + suffix, origin + "/repositories/9223372036854775807/" + suffix, origin + path}
			for _, target := range accepted {
				page, found, err := nextPage([]string{"<" + target + "?per_page=100&page=2>; rel=next"}, base, path, 1)
				if err != nil || !found || page != 2 {
					t.Errorf("valid canonical path rejected: %s: %d %v %v", target, page, found, err)
				}
			}
			for _, id := range []string{"", "0", "00", "01", "-1", "+1", "1.0", "abc", "9223372036854775808", "%31", "%2F1", "1%2F", "%2531"} {
				target := origin + "/repositories/" + id + "/" + suffix
				if _, _, err := nextPage([]string{"<" + target + "?per_page=100&page=2>; rel=next"}, base, path, 1); err == nil {
					t.Errorf("malformed numeric identity accepted: %q", target)
				}
			}
			invalid := []string{
				origin + linkPath + "/", origin + linkPath + "/extra", origin + strings.Replace(linkPath, "/7/", "/8/", 1), origin + strings.Replace(linkPath, "repositories", "repos", 1),
				origin + strings.Replace(linkPath, "/7/", "/%37/", 1), origin + "/repositories/1418705294/pulls/7/reviews/../comments", origin + "/repositories/1418705294/other/7/comments",
				origin + strings.Replace(linkPath, "/", "//", 1), origin + "/other" + linkPath, "https://foreign.example" + linkPath, "http://api.github.com" + linkPath,
				origin + linkPath + "#fragment", "https://user@" + strings.TrimPrefix(origin, "https://") + linkPath,
			}
			if strings.Contains(base, "/api/v3") {
				invalid = append(invalid, "https://github.example"+linkPath, "https://github.example/api/v30"+linkPath)
			}
			for _, target := range invalid { // Add query before fragment so the fragment remains independently invalid.
				target = strings.Replace(target, "#fragment", "?per_page=100&page=2#fragment", 1)
				if !strings.Contains(target, "?") {
					target += "?per_page=100&page=2"
				}
				if _, _, err := nextPage([]string{"<" + target + ">; rel=next"}, base, path, 1); err == nil {
					t.Errorf("cross-scope numeric link accepted: %q", target)
				}
			}
		}
	}
}

func TestGitHubMalformedNumericRepositoryLinkNeverCompletesPartialCollection(t *testing.T) {
	for _, kind := range []string{"issues", "reviews", "review-comments"} {
		for _, bad := range []string{"repositories/01/", "repositories/9223372036854775808/", "repositories/%31/", "repositories/1/other/"} {
			t.Run(kind+"/"+bad, func(t *testing.T) {
				var requests atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					w.Header().Set("Link", "<"+serverURL(r)+"/"+bad+numericPaginationSuffix(kind)+"?page=2&per_page=100>; rel=next")
					_, _ = ioWriteString(w, "["+numericPaginationItem(kind, 1)+"]")
				}))
				defer server.Close()
				bodies, err := readNumericPaginationCollection(t, testGitHub(server, ""), kind)
				if !hasCode(err, "provider_invalid_response") || bodies != nil || requests.Load() != 1 {
					t.Fatal("malformed numeric link followed or partial returned", bodies, requests.Load(), err)
				}
			})
		}
	}
}

func TestGitHubNumericRepositoryLinkNeverFetchesForeignOrigin(t *testing.T) {
	var foreignRequests atomic.Int64
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { foreignRequests.Add(1); _, _ = ioWriteString(w, "[]") }))
	defer foreign.Close()
	for _, kind := range []string{"issues", "reviews", "review-comments"} {
		t.Run(kind, func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Link", "<"+foreign.URL+"/repositories/1418705294/"+numericPaginationSuffix(kind)+"?per_page=100&page=2>; rel=next")
				_, _ = ioWriteString(w, "["+numericPaginationItem(kind, 1)+"]")
			}))
			defer server.Close()
			bodies, err := readNumericPaginationCollection(t, testGitHub(server, "synthetic-pagination-token"), kind)
			if !hasCode(err, "provider_invalid_response") || bodies != nil || requests.Load() != 1 || foreignRequests.Load() != 0 {
				t.Fatal("foreign numeric Link fetched or partial collection returned", bodies, requests.Load(), foreignRequests.Load(), err)
			}
		})
	}
}

func TestGitHubPaginationRejectsSkippedPageWithoutReturningPartialCollection(t *testing.T) {
	for _, kind := range []string{"issues", "reviews", "review-comments"} {
		for _, numeric := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/numeric=%v", kind, numeric), func(t *testing.T) {
				suffix := numericPaginationSuffix(kind)
				path := "/repos/owner/repo/" + suffix
				var requests atomic.Int64
				var requestedPageThree atomic.Bool
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if r.Method != http.MethodGet || r.URL.Path != path || r.URL.RawPath != "" || r.URL.Query().Get("per_page") != "100" || r.Header.Get("Authorization") != "Bearer synthetic-pagination-token" {
						t.Errorf("unexpected collection request: %s %s", r.Method, r.URL.String())
						http.NotFound(w, r)
						return
					}
					switch r.URL.Query().Get("page") {
					case "1":
						targetPath := path
						if numeric {
							targetPath = "/repositories/1418705294/" + suffix
						}
						w.Header().Set("Link", "<"+serverURL(r)+targetPath+"?page=3&per_page=100>; rel=next")
						_, _ = ioWriteString(w, "["+numericPaginationItem(kind, 1)+"]")
					case "3":
						requestedPageThree.Store(true)
						_, _ = ioWriteString(w, "["+numericPaginationItem(kind, 3)+"]")
					default:
						t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				bodies, err := readNumericPaginationCollection(t, testGitHub(server, "synthetic-pagination-token"), kind)
				if !hasCode(err, "provider_invalid_response") || bodies != nil || requests.Load() != 1 || requestedPageThree.Load() {
					t.Fatalf("skipped page was followed or collection treated as complete: bodies=%v err=%v requests=%d page3=%v", bodies, err, requests.Load(), requestedPageThree.Load())
				}
			})
		}
	}
}

func TestGitHubPaginationAcceptsThreeAdjacentPagesOnOriginalCollection(t *testing.T) {
	for _, kind := range []string{"issues", "reviews", "review-comments"} {
		for _, numeric := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/numeric=%v", kind, numeric), func(t *testing.T) {
				suffix := numericPaginationSuffix(kind)
				path := "/repos/owner/repo/" + suffix
				var requests atomic.Int64
				var pages [3]atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					index := requests.Add(1) - 1
					page, err := strconv.ParseInt(r.URL.Query().Get("page"), 10, 64)
					if err != nil || index >= 3 || page < 1 || page > 3 || r.Method != http.MethodGet || r.URL.Path != path || r.URL.RawPath != "" || r.URL.Query().Get("per_page") != "100" || r.Header.Get("Authorization") != "Bearer synthetic-pagination-token" {
						t.Errorf("unexpected collection request: %s %s", r.Method, r.URL.String())
						http.NotFound(w, r)
						return
					}
					pages[index].Store(page)
					if page < 3 {
						targetPath := path
						if numeric {
							targetPath = "/repositories/1418705294/" + suffix
						}
						w.Header().Set("Link", fmt.Sprintf("<%s%s?page=%d&per_page=100>; rel=next", serverURL(r), targetPath, page+1))
					}
					item := numericPaginationItem(kind, int(page))
					if page == 3 {
						item = strings.Replace(item, "page one", "page three", 1)
					}
					_, _ = ioWriteString(w, "["+item+"]")
				}))
				defer server.Close()
				bodies, err := readNumericPaginationCollection(t, testGitHub(server, "synthetic-pagination-token"), kind)
				if err != nil || strings.Join(bodies, ",") != "page one,page two,page three" || requests.Load() != 3 || pages[0].Load() != 1 || pages[1].Load() != 2 || pages[2].Load() != 3 {
					t.Fatalf("adjacent pagination lost data or fetched alias: bodies=%v err=%v requests=%d pages=%d,%d,%d", bodies, err, requests.Load(), pages[0].Load(), pages[1].Load(), pages[2].Load())
				}
			})
		}
	}
}
