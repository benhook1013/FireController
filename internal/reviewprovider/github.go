package reviewprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/strictjson"
)

const (
	githubAPIURL       = "https://api.github.com"
	githubAPIHost      = "api.github.com"
	githubRequestLimit = 5 * time.Second
	githubPageSize     = 100
	githubMaxPages     = 100
	githubMaxItems     = 10000
	githubMaxBody      = 8 << 20
	githubMaxAggregate = 16 << 20
	githubMaxText      = 64 << 10
)

// GitHub is a bounded adapter for explicitly selected GitHub resources. It
// never discovers repositories, retries requests, or chooses review targets.
type GitHub struct {
	token      string
	baseURL    string
	httpClient *http.Client
}

// NewGitHub creates the production GitHub API adapter. An empty token permits
// public reads; operations that require authentication will fail at GitHub.
func NewGitHub(token string) (*GitHub, error) {
	if !validToken(token) {
		return nil, &Error{Code: "provider_unavailable", Message: "The provider credential is invalid."}
	}
	client := &http.Client{
		Timeout: githubRequestLimit,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DisableKeepAlives:     true,
			TLSHandshakeTimeout:   githubRequestLimit,
			ResponseHeaderTimeout: githubRequestLimit,
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &GitHub{token: token, baseURL: githubAPIURL, httpClient: client}, nil
}

func validToken(token string) bool {
	if len(token) > 4096 || !utf8.ValidString(token) {
		return false
	}
	for i := 0; i < len(token); i++ {
		if token[i] < 0x21 || token[i] > 0x7e {
			return false
		}
	}
	return true
}

func validRepositorySelector(repository string) bool {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || len(part) > 100 || part == "." || part == ".." {
			return false
		}
		for i := 0; i < len(part); i++ {
			c := part[i]
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func validPullNumber(number int64) bool { return number > 0 && number <= 2147483647 }

func (g *GitHub) CurrentActor(ctx context.Context) (Author, error) {
	var raw githubActor
	if err := g.getJSON(ctx, "/user", nil, &raw); err != nil {
		return Author{}, err
	}
	author, err := normalizeActor(raw)
	if err != nil {
		return Author{}, invalidResponse(false, 200)
	}
	return author, nil
}

func (g *GitHub) Repository(ctx context.Context, id int64) (Repository, error) {
	if id <= 0 {
		return Repository{}, invalidSelector()
	}
	var raw githubRepository
	if err := g.getJSON(ctx, "/repositories/"+strconv.FormatInt(id, 10), nil, &raw); err != nil {
		return Repository{}, err
	}
	repository, err := normalizeRepository(raw, id)
	if err != nil {
		return Repository{}, invalidResponse(false, 200)
	}
	return repository, nil
}

func (g *GitHub) Pull(ctx context.Context, repository string, number int64) (Pull, error) {
	if !validRepositorySelector(repository) || !validPullNumber(number) {
		return Pull{}, invalidSelector()
	}
	path := "/repos/" + repository + "/pulls/" + strconv.FormatInt(number, 10)
	var raw githubPull
	if err := g.getJSON(ctx, path, nil, &raw); err != nil {
		return Pull{}, err
	}
	pull, err := normalizePull(raw, number)
	if err != nil {
		return Pull{}, invalidResponse(false, 200)
	}
	return pull, nil
}

func (g *GitHub) IssueComments(ctx context.Context, repository string, number int64) ([]IssueComment, error) {
	if !validRepositorySelector(repository) || !validPullNumber(number) {
		return nil, invalidSelector()
	}
	path := "/repos/" + repository + "/issues/" + strconv.FormatInt(number, 10) + "/comments"
	var raw []githubIssueComment
	if err := g.getPages(ctx, path, &raw); err != nil {
		return nil, err
	}
	result := make([]IssueComment, 0, len(raw))
	for _, item := range raw {
		comment, err := normalizeIssueComment(item)
		if err != nil {
			return nil, invalidResponse(false, 200)
		}
		result = append(result, comment)
	}
	return result, nil
}

// CheckpointComments collects the complete selected history before projecting
// comments attributable to the original stable actor. Foreign prose and cosmetic
// metadata do not establish checkpoint identity; pagination, wire shape, global
// comment IDs and stable author identities remain authoritative for every row.
// Other evidence readers keep the stricter IssueComments contract.
func (g *GitHub) CheckpointComments(ctx context.Context, repository string, number int64, expectedActor Author) ([]IssueComment, error) {
	if !validRepositorySelector(repository) || !validPullNumber(number) {
		return nil, invalidSelector()
	}
	if _, err := normalizeActor(githubActor{ID: expectedActor.ID, Login: expectedActor.Login, Type: expectedActor.Kind}); err != nil {
		return nil, invalidSelector()
	}
	path := "/repos/" + repository + "/issues/" + strconv.FormatInt(number, 10) + "/comments"
	var raw []githubIssueComment
	if err := g.getPages(ctx, path, &raw); err != nil {
		return nil, err
	}
	result := make([]IssueComment, 0, len(raw))
	seen := make(map[int64]bool, len(raw))
	for _, item := range raw {
		if item.ID <= 0 || seen[item.ID] || !validCheckpointActorIdentity(Author{ID: item.User.ID, Kind: item.User.Type}) {
			return nil, invalidResponse(false, http.StatusOK)
		}
		seen[item.ID] = true
		if item.User.ID != expectedActor.ID || item.User.Type != expectedActor.Kind {
			continue
		}
		comment, err := normalizeIssueComment(item)
		if err != nil {
			return nil, invalidResponse(false, http.StatusOK)
		}
		result = append(result, comment)
	}
	return result, nil
}

func (g *GitHub) Reviews(ctx context.Context, repository string, number int64) ([]HostedReview, error) {
	if !validRepositorySelector(repository) || !validPullNumber(number) {
		return nil, invalidSelector()
	}
	path := "/repos/" + repository + "/pulls/" + strconv.FormatInt(number, 10) + "/reviews"
	var raw []githubHostedReview
	if err := g.getPages(ctx, path, &raw); err != nil {
		return nil, err
	}
	result := make([]HostedReview, 0, len(raw))
	for _, item := range raw {
		review, err := normalizeHostedReview(item)
		if err != nil {
			return nil, invalidResponse(false, 200)
		}
		result = append(result, review)
	}
	return result, nil
}

func (g *GitHub) ReviewComments(ctx context.Context, repository string, number int64) ([]ReviewComment, error) {
	if !validRepositorySelector(repository) || !validPullNumber(number) {
		return nil, invalidSelector()
	}
	path := "/repos/" + repository + "/pulls/" + strconv.FormatInt(number, 10) + "/comments"
	var raw []githubReviewComment
	if err := g.getPages(ctx, path, &raw); err != nil {
		return nil, err
	}
	result := make([]ReviewComment, 0, len(raw))
	for _, item := range raw {
		comment, err := normalizeReviewComment(item)
		if err != nil {
			return nil, invalidResponse(false, 200)
		}
		result = append(result, comment)
	}
	return result, nil
}

func (g *GitHub) PostIssueComment(ctx context.Context, repository string, number int64, body string) (IssueComment, error) {
	if !validRepositorySelector(repository) || !validPullNumber(number) || !validBody(body) {
		return IssueComment{}, invalidSelector()
	}
	encoded, err := json.Marshal(struct {
		Body string `json:"body"`
	}{Body: body})
	if err != nil {
		return IssueComment{}, invalidSelector()
	}
	path := "/repos/" + repository + "/issues/" + strconv.FormatInt(number, 10) + "/comments"
	var raw githubIssueComment
	if err := g.requestJSON(ctx, http.MethodPost, path, nil, encoded, http.StatusCreated, &raw); err != nil {
		return IssueComment{}, err
	}
	comment, err := normalizeIssueComment(raw)
	if err != nil || comment.Body != body {
		return IssueComment{}, invalidResponse(true, http.StatusCreated)
	}
	return comment, nil
}

func (g *GitHub) getJSON(ctx context.Context, path string, query url.Values, dst any) error {
	return g.requestJSON(ctx, http.MethodGet, path, query, nil, http.StatusOK, dst)
}

func (g *GitHub) requestJSON(ctx context.Context, method, path string, query url.Values, body []byte, expectedStatus int, dst any) error {
	if ctx == nil || ctx.Err() != nil {
		return providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
	}
	if g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) || path == "" || path[0] != '/' {
		return providerError("provider_unavailable", "The provider adapter is unavailable.", 0, 0, false)
	}
	requestURL := strings.TrimRight(g.baseURL, "/") + path
	if len(query) != 0 {
		requestURL += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL, bytes.NewReader(body))
	if err != nil {
		return providerError("provider_unavailable", "The provider request could not be prepared.", 0, 0, false)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "FireController")
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	if g.token != "" {
		request.Header.Set("Authorization", "Bearer "+g.token)
	}
	response, err := g.httpClient.Do(request)
	if err != nil {
		return providerError("provider_unavailable", "The provider request failed.", 0, 0, method == http.MethodPost)
	}
	defer response.Body.Close()
	if response.StatusCode != expectedStatus {
		return statusError(response, method == http.MethodPost)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, githubMaxBody+1))
	if err != nil || len(data) > githubMaxBody {
		return invalidResponse(method == http.MethodPost, response.StatusCode)
	}
	if err := decodeProviderJSON(data, dst); err != nil {
		return invalidResponse(method == http.MethodPost, response.StatusCode)
	}
	return nil
}

func (g *GitHub) getPages(ctx context.Context, path string, dst any) error {
	if ctx == nil || ctx.Err() != nil {
		return providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
	}
	if g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) || path == "" || path[0] != '/' {
		return providerError("provider_unavailable", "The provider adapter is unavailable.", 0, 0, false)
	}
	var aggregate int64
	var totalItems int
	var page int64 = 1
	combined := make([]json.RawMessage, 0)
	for pageCount := 0; pageCount < githubMaxPages; pageCount++ {
		query := url.Values{"page": {strconv.FormatInt(page, 10)}, "per_page": {strconv.Itoa(githubPageSize)}}
		var rows []json.RawMessage
		data, headers, status, err := g.requestBytes(ctx, http.MethodGet, path, query, nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return statusError(&http.Response{StatusCode: status, Header: headers}, false)
		}
		aggregate += int64(len(data))
		if aggregate > githubMaxAggregate {
			return invalidResponse(false, status)
		}
		if err := strictjson.Validate(data); err != nil || json.Unmarshal(data, &rows) != nil || rows == nil {
			return invalidResponse(false, status)
		}
		if len(rows) > githubPageSize || totalItems > githubMaxItems-len(rows) {
			return invalidResponse(false, status)
		}
		totalItems += len(rows)
		combined = append(combined, rows...)
		next, exists, err := nextPage(headers.Values("Link"), g.baseURL, path, page)
		if err != nil {
			return invalidResponse(false, status)
		}
		if !exists {
			break
		}
		if pageCount == githubMaxPages-1 {
			return invalidResponse(false, status)
		}
		page = next
	}
	encoded, err := json.Marshal(combined)
	if err != nil || len(encoded) > githubMaxAggregate {
		return invalidResponse(false, http.StatusOK)
	}
	if err := decodeProviderJSON(encoded, dst); err != nil {
		return invalidResponse(false, http.StatusOK)
	}
	return nil
}

func (g *GitHub) requestBytes(ctx context.Context, method, path string, query url.Values, body []byte) ([]byte, http.Header, int, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, nil, 0, providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
	}
	requestURL := strings.TrimRight(g.baseURL, "/") + path + "?" + query.Encode()
	request, err := http.NewRequestWithContext(ctx, method, requestURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, 0, providerError("provider_unavailable", "The provider request could not be prepared.", 0, 0, false)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "FireController")
	if g.token != "" {
		request.Header.Set("Authorization", "Bearer "+g.token)
	}
	response, err := g.httpClient.Do(request)
	if err != nil {
		return nil, nil, 0, providerError("provider_unavailable", "The provider request failed.", 0, 0, false)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, response.Header.Clone(), response.StatusCode, nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, githubMaxBody+1))
	if err != nil || len(data) > githubMaxBody {
		return nil, nil, 0, invalidResponse(false, response.StatusCode)
	}
	return data, response.Header.Clone(), response.StatusCode, nil
}

func statusError(response *http.Response, mutation bool) error {
	status := response.StatusCode
	retry := retryAfter(response.Header, time.Now())
	unknown := mutation && (status == http.StatusRequestTimeout || status >= 200 && status < 300 || status >= 300 && status < 400 || status >= 500)
	switch {
	case status == http.StatusNotFound:
		return providerError("provider_not_found", "The requested provider resource was not found.", status, retry, false)
	case status == http.StatusTooManyRequests:
		return providerError("provider_rate_limited", "The provider rate limit was reached.", status, retry, unknown)
	case status == http.StatusForbidden && (response.Header.Get("X-RateLimit-Remaining") == "0" || response.Header.Get("Retry-After") != ""):
		return providerError("provider_rate_limited", "The provider rate limit was reached.", status, retry, unknown)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return providerError("provider_unauthorized", "The provider rejected authorization.", status, retry, false)
	case status >= 500:
		return providerError("provider_unavailable", "The provider is temporarily unavailable.", status, retry, unknown)
	case status == http.StatusRequestTimeout:
		return providerError("provider_unavailable", "The provider request timed out.", status, retry, unknown)
	case status >= 300 && status < 400:
		return providerError("provider_unavailable", "The provider returned an unsupported redirect.", status, retry, unknown)
	case status >= 200 && status < 300:
		return providerError("provider_invalid_response", "The provider returned an unexpected success response.", status, retry, unknown)
	default:
		return providerError("provider_unavailable", "The provider request was rejected.", status, retry, false)
	}
}

func retryAfter(header http.Header, now time.Time) int64 {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value != "" {
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
			return minInt64(seconds, 86400)
		}
		if parsed, err := http.ParseTime(value); err == nil {
			seconds := int64(parsed.Sub(now).Seconds())
			if seconds > 0 {
				return minInt64(seconds, 86400)
			}
			return 0
		}
	}
	if header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(strings.TrimSpace(header.Get("X-RateLimit-Reset")), 10, 64); err == nil && reset > 0 {
			seconds := reset - now.Unix()
			if seconds > 0 {
				return minInt64(seconds, 86400)
			}
		}
	}
	return 0
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func nextPage(linkHeaders []string, baseURL, path string, current int64) (int64, bool, error) {
	return nextPageWithQuery(linkHeaders, baseURL, path, current, nil)
}

// Collection-specific fixed selectors must survive pagination exactly; a Link
// may select only the adjacent page, never change the original request scope.
func nextPageWithQuery(linkHeaders []string, baseURL, path string, current int64, fixed url.Values) (int64, bool, error) {
	for key, values := range fixed {
		if key == "" || key == "page" || key == "per_page" || len(values) != 1 || values[0] == "" {
			return 0, false, errors.New("invalid fixed query")
		}
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return 0, false, errors.New("invalid base")
	}
	basePath := strings.TrimRight(base.Path, "/")
	var next int64
	found := false
	for _, header := range linkHeaders {
		if strings.TrimSpace(header) == "" {
			return 0, false, errors.New("empty link header")
		}
		links, err := parseLinkHeader(header)
		if err != nil {
			return 0, false, err
		}
		for _, link := range links {
			if !link.next {
				continue
			}
			if found {
				return 0, false, errors.New("ambiguous next")
			}
			found = true
			target, err := url.Parse(link.target)
			if err != nil || target.Scheme != base.Scheme || !strings.EqualFold(target.Host, base.Host) || target.User != nil || target.Fragment != "" || !validPaginationPath(target, basePath, path) {
				return 0, false, errors.New("untrusted next")
			}
			query, err := url.ParseQuery(target.RawQuery)
			if err != nil || len(query) != 2+len(fixed) || len(query["page"]) != 1 || len(query["per_page"]) != 1 || query.Get("per_page") != strconv.Itoa(githubPageSize) {
				return 0, false, errors.New("invalid next query")
			}
			for key, values := range fixed {
				if len(query[key]) != 1 || query.Get(key) != values[0] {
					return 0, false, errors.New("changed fixed query")
				}
			}
			page, err := strconv.ParseInt(query.Get("page"), 10, 64)
			if err != nil || strconv.FormatInt(page, 10) != query.Get("page") || page != current+1 || page <= 0 || page > githubMaxPages {
				return 0, false, errors.New("nonadvancing next")
			}
			next = page
		}
	}
	return next, found, nil
}

// GitHub may canonicalize a named repository Link to its numeric identity.
// Accept only that collection alias, and use it solely to validate a page
// number: getPages always reconstructs the original named request path.
func validPaginationPath(target *url.URL, basePath, path string) bool {
	// An explicitly selected escaped branch can contain a slash inside its
	// selector. Accept its exact wire path without accepting another encoding
	// or a decoded path that changes those selector boundaries.
	if target.EscapedPath() == basePath+path {
		return true
	}
	if target.EscapedPath() != target.Path {
		return false
	}
	if !strings.HasPrefix(path, "/repos/") {
		return false
	}
	parts := strings.SplitN(strings.TrimPrefix(path, "/repos/"), "/", 3)
	if len(parts) != 3 || !validRepositorySelector(parts[0]+"/"+parts[1]) || parts[2] == "" {
		return false
	}
	prefix := basePath + "/repositories/"
	if !strings.HasPrefix(target.Path, prefix) {
		return false
	}
	id, suffix, found := strings.Cut(strings.TrimPrefix(target.Path, prefix), "/")
	number, err := strconv.ParseInt(id, 10, 64)
	return found && err == nil && number > 0 && strconv.FormatInt(number, 10) == id && suffix == parts[2]
}

type githubLink struct {
	target string
	next   bool
}

// Parse the Link framing before deciding which relation to follow. Invalid
// relation syntax must not silently turn a partial collection into a final page.
func parseLinkHeader(header string) ([]githubLink, error) {
	var links []githubLink
	position := 0
	skipSpace := func() {
		for position < len(header) && (header[position] == ' ' || header[position] == '\t') {
			position++
		}
	}
	invalid := func() ([]githubLink, error) { return nil, errors.New("malformed link entry") }
	for {
		skipSpace()
		if position >= len(header) || header[position] != '<' {
			return invalid()
		}
		position++
		start := position
		for position < len(header) && header[position] != '>' {
			if header[position] <= ' ' || header[position] == '<' || header[position] == 0x7f {
				return invalid()
			}
			position++
		}
		if position >= len(header) {
			return invalid()
		}
		link := githubLink{target: header[start:position]}
		if _, err := url.Parse(link.target); err != nil {
			return invalid()
		}
		position++
		relSeen := false
		for {
			skipSpace()
			if position == len(header) || header[position] == ',' {
				break
			}
			if header[position] != ';' {
				return invalid()
			}
			position++
			skipSpace()
			start = position
			for position < len(header) && linkTokenByte(header[position]) {
				position++
			}
			if position == start {
				return invalid()
			}
			name := header[start:position]
			skipSpace()
			hasValue := position < len(header) && header[position] == '='
			value := ""
			if hasValue {
				position++
				skipSpace()
				if position >= len(header) {
					return invalid()
				}
				if header[position] == '"' {
					position++
					var decoded strings.Builder
					for position < len(header) && header[position] != '"' {
						c := header[position]
						if c == '\\' {
							position++
							if position == len(header) {
								return invalid()
							}
							c = header[position]
						}
						if c < 0x20 && c != '\t' || c == 0x7f {
							return invalid()
						}
						decoded.WriteByte(c)
						position++
					}
					if position == len(header) {
						return invalid()
					}
					position++
					value = decoded.String()
				} else {
					start = position
					for position < len(header) && linkTokenByte(header[position]) {
						position++
					}
					if position == start {
						return invalid()
					}
					value = header[start:position]
				}
			}
			if strings.EqualFold(name, "rel") {
				if relSeen || !hasValue || strings.Trim(value, " ") == "" {
					return invalid()
				}
				relSeen = true
				for _, relation := range strings.Split(value, " ") {
					if relation == "" {
						continue
					}
					if !validLinkRelation(relation) {
						return invalid()
					}
					if strings.EqualFold(relation, "next") {
						if link.next {
							return invalid()
						}
						link.next = true
					}
				}
			}
		}
		links = append(links, link)
		if position == len(header) {
			return links, nil
		}
		position++ // Consume the comma; the next iteration requires another link.
	}
}

func linkTokenByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))
}

func validLinkRelation(value string) bool {
	registered := true
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && (c >= '0' && c <= '9' || c == '.' || c == '-')) {
			registered = false
		}
		if c <= ' ' || c == 0x7f {
			return false
		}
	}
	if registered {
		return true
	}
	// Extension relations use absolute URIs; they never select a next page.
	parsed, err := url.Parse(value)
	return err == nil && parsed.IsAbs()
}

func validBaseURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" {
		return false
	}
	if parsed.Scheme == "https" && parsed.Host == githubAPIHost {
		return true
	}
	host := strings.ToLower(parsed.Hostname())
	return parsed.Scheme == "http" && (host == "127.0.0.1" || host == "localhost" || host == "::1") && parsed.Port() != ""
}

func providerError(code, message string, status int, retry int64, unknown bool) *Error {
	return &Error{Code: code, Message: message, Status: status, RetryAfterSeconds: retry, OutcomeUnknown: unknown}
}

func invalidSelector() *Error {
	return providerError("provider_invalid_response", "The provider request parameters are invalid.", 0, 0, false)
}

func invalidResponse(unknown bool, status int) *Error {
	return providerError("provider_invalid_response", "The provider returned an invalid response.", status, 0, unknown)
}

// GitHub wire representations. Unknown fields are ignored only after strict
// JSON, duplicate-key and all recognized-field shape validation.
type githubActor struct {
	ID    int64  `json:"id,required"`
	Login string `json:"login,required"`
	Type  string `json:"type,required"`
}

type githubRepository struct {
	ID            int64  `json:"id,required"`
	FullName      string `json:"full_name,required"`
	DefaultBranch string `json:"default_branch,required"`
}

type githubPull struct {
	Title  string `json:"title,required"`
	Number int64  `json:"number,required"`
	State  string `json:"state,required"`
	Merged bool   `json:"merged,required"`
	Head   struct {
		SHA  string            `json:"sha,required"`
		Repo *githubRepository `json:"repo,required"`
	} `json:"head,required"`
	Base struct {
		SHA  string           `json:"sha,required"`
		Ref  string           `json:"ref,required"`
		Repo githubRepository `json:"repo,required"`
	} `json:"base,required"`
	ChangedFiles int64 `json:"changed_files,required"`
}

type githubIssueComment struct {
	ID        int64       `json:"id,required"`
	Body      string      `json:"body,required"`
	User      githubActor `json:"user,required"`
	CreatedAt string      `json:"created_at,required"`
	UpdatedAt string      `json:"updated_at,required"`
}

type githubHostedReview struct {
	ID          int64       `json:"id,required"`
	Body        string      `json:"body,required"`
	State       string      `json:"state,required"`
	CommitID    string      `json:"commit_id,required"`
	User        githubActor `json:"user,required"`
	SubmittedAt *string     `json:"submitted_at,required"`
}

type githubReviewComment struct {
	InReplyToID       *int64      `json:"in_reply_to_id"`
	ID                int64       `json:"id,required"`
	ReviewID          int64       `json:"pull_request_review_id,required"`
	Body              string      `json:"body,required"`
	User              githubActor `json:"user,required"`
	Path              string      `json:"path,required"`
	Line              *int64      `json:"line"`
	StartLine         *int64      `json:"start_line"`
	OriginalLine      *int64      `json:"original_line"`
	OriginalStartLine *int64      `json:"original_start_line"`
	Side              *string     `json:"side"`
	StartSide         *string     `json:"start_side"`
	CommitID          string      `json:"commit_id,required"`
	OriginalCommit    string      `json:"original_commit_id,required"`
	CreatedAt         string      `json:"created_at,required"`
	UpdatedAt         string      `json:"updated_at,required"`
}

func decodeProviderJSON(data []byte, dst any) error {
	if err := strictjson.Validate(data); err != nil {
		return err
	}
	if err := validateKnownShape(data, reflect.TypeOf(dst)); err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}

func validateKnownShape(data []byte, typ reflect.Type) error {
	for typ.Kind() == reflect.Pointer {
		if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
			return nil
		}
		typ = typ.Elem()
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("null known field")
	}
	switch typ.Kind() {
	case reflect.Struct:
		fields, ok := jsonObject(data)
		if !ok {
			return errors.New("expected object")
		}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			options := strings.Split(field.Tag.Get("json"), ",")
			name := options[0]
			if name == "" {
				name = field.Name
			}
			if name == "-" {
				continue
			}
			for supplied := range fields {
				if supplied != name && strings.EqualFold(supplied, name) {
					return errors.New("noncanonical known field")
				}
			}
			raw, found := fields[name]
			required := false
			for _, option := range options[1:] {
				required = required || option == "required"
			}
			if !found && required {
				return errors.New("missing required field")
			}
			if found {
				if err := validateKnownShape(raw, field.Type); err != nil {
					return err
				}
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil || values == nil {
			return errors.New("expected array")
		}
		for _, value := range values {
			if err := validateKnownShape(value, typ.Elem()); err != nil {
				return err
			}
		}
	case reflect.String:
		var value string
		if err := json.Unmarshal(data, &value); err != nil || !utf8.ValidString(value) {
			return errors.New("expected string")
		}
	case reflect.Bool:
		var value bool
		if err := json.Unmarshal(data, &value); err != nil {
			return errors.New("expected boolean")
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		var value json.Number
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return errors.New("expected integer")
		}
		if _, err := value.Int64(); err != nil {
			return errors.New("expected integer")
		}
	}
	return nil
}

func jsonObject(data []byte) (map[string]json.RawMessage, bool) {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(data, &value); err != nil || value == nil {
		return nil, false
	}
	return value, true
}

func normalizeActor(raw githubActor) (Author, error) {
	if raw.ID <= 0 || !validIdentifierText(raw.Login, 100) || !validIdentifierText(raw.Type, 32) {
		return Author{}, errors.New("invalid actor")
	}
	return Author{ID: raw.ID, Login: raw.Login, Kind: raw.Type}, nil
}

func normalizeRepository(raw githubRepository, expectedID int64) (Repository, error) {
	if raw.ID <= 0 || expectedID > 0 && raw.ID != expectedID || !validRepositorySelector(raw.FullName) || !validPlainText(raw.DefaultBranch, 255) || strings.TrimSpace(raw.DefaultBranch) == "" {
		return Repository{}, errors.New("invalid repository")
	}
	return Repository{ID: raw.ID, FullName: raw.FullName, DefaultBranch: raw.DefaultBranch}, nil
}

func normalizePull(raw githubPull, expectedNumber int64) (Pull, error) {
	if !validPlainText(raw.Title, 4096) || strings.TrimSpace(raw.Title) == "" || raw.Number != expectedNumber || raw.ChangedFiles < 0 || raw.ChangedFiles > 1000000 || raw.State != "open" && raw.State != "closed" || raw.Merged && raw.State != "closed" || raw.Base.Repo.ID <= 0 || !validSHA(raw.Head.SHA) || !validSHA(raw.Base.SHA) || !validPlainText(raw.Base.Ref, 255) || strings.TrimSpace(raw.Base.Ref) == "" {
		return Pull{}, errors.New("invalid pull")
	}
	if _, err := normalizeRepository(raw.Base.Repo, 0); err != nil {
		return Pull{}, errors.New("invalid base repository")
	}
	if raw.Head.Repo != nil {
		if _, err := normalizeRepository(*raw.Head.Repo, 0); err != nil {
			return Pull{}, errors.New("invalid head repository")
		}
	}
	return Pull{Title: raw.Title, Number: raw.Number, RepositoryID: raw.Base.Repo.ID, State: raw.State, Merged: raw.Merged, HeadSHA: raw.Head.SHA, BaseSHA: raw.Base.SHA, BaseRef: raw.Base.Ref, ChangedFiles: raw.ChangedFiles}, nil
}

func normalizeIssueComment(raw githubIssueComment) (IssueComment, error) {
	actor, err := normalizeActor(raw.User)
	if err != nil || raw.ID <= 0 || !validBody(raw.Body) {
		return IssueComment{}, errors.New("invalid comment")
	}
	created, err := parseTimestamp(raw.CreatedAt)
	if err != nil {
		return IssueComment{}, err
	}
	updated, err := parseTimestamp(raw.UpdatedAt)
	if err != nil {
		return IssueComment{}, err
	}
	return IssueComment{ID: raw.ID, Body: raw.Body, Author: actor, CreatedAt: created, UpdatedAt: updated}, nil
}

func normalizeHostedReview(raw githubHostedReview) (HostedReview, error) {
	actor, err := normalizeActor(raw.User)
	if err != nil || raw.ID <= 0 || !validBody(raw.Body) || !validSHA(raw.CommitID) {
		return HostedReview{}, errors.New("invalid review")
	}
	state := strings.ToLower(raw.State)
	if state != "approved" && state != "changes_requested" && state != "commented" && state != "pending" && state != "dismissed" {
		return HostedReview{}, errors.New("invalid review state")
	}
	var submitted time.Time
	if raw.SubmittedAt == nil {
		if state != "pending" {
			return HostedReview{}, errors.New("missing review time")
		}
	} else {
		submitted, err = parseTimestamp(*raw.SubmittedAt)
		if err != nil || state == "pending" {
			return HostedReview{}, errors.New("invalid review time")
		}
	}
	return HostedReview{ID: raw.ID, Body: raw.Body, State: state, HeadSHA: raw.CommitID, Author: actor, SubmittedAt: submitted}, nil
}

func normalizeReviewComment(raw githubReviewComment) (ReviewComment, error) {
	actor, err := normalizeActor(raw.User)
	if err != nil || raw.ID <= 0 || raw.ReviewID <= 0 || !validBody(raw.Body) || !validRepoPath(raw.Path) || !validSHA(raw.CommitID) || !validSHA(raw.OriginalCommit) {
		return ReviewComment{}, errors.New("invalid review comment")
	}
	created, err := parseTimestamp(raw.CreatedAt)
	if err != nil {
		return ReviewComment{}, err
	}
	updated, err := parseTimestamp(raw.UpdatedAt)
	if err != nil {
		return ReviewComment{}, err
	}
	for _, line := range []*int64{raw.Line, raw.StartLine, raw.OriginalLine, raw.OriginalStartLine} {
		if !validReviewCommentLine(line) {
			return ReviewComment{}, errors.New("invalid comment line")
		}
	}
	var side, startSide string
	for _, pair := range []struct {
		source *string
		target *string
	}{{raw.Side, &side}, {raw.StartSide, &startSide}} {
		if pair.source != nil {
			if *pair.source != "LEFT" && *pair.source != "RIGHT" {
				return ReviewComment{}, errors.New("invalid comment side")
			}
			*pair.target = *pair.source
		}
	}
	var parent int64
	if raw.InReplyToID != nil {
		if *raw.InReplyToID <= 0 || *raw.InReplyToID == raw.ID {
			return ReviewComment{}, errors.New("invalid reply parent")
		}
		parent = *raw.InReplyToID
	}
	return ReviewComment{InReplyToID: parent, ID: raw.ID, ReviewID: raw.ReviewID, Body: raw.Body, Author: actor, Path: raw.Path, Line: raw.Line, StartLine: raw.StartLine, OriginalLine: raw.OriginalLine, OriginalStartLine: raw.OriginalStartLine, Side: side, StartSide: startSide, HeadSHA: raw.CommitID, OriginalHeadSHA: raw.OriginalCommit, CreatedAt: created, UpdatedAt: updated}, nil
}

func validReviewCommentLine(line *int64) bool {
	return line == nil || *line > 0 && *line <= 1<<31-1
}

// Empty normalized sides represent omitted/null provider metadata. These
// checks preserve facts without inferring a range or a relationship to the diff.
func validReviewCommentLocation(comment ReviewComment) bool {
	return validReviewCommentLine(comment.Line) && validReviewCommentLine(comment.StartLine) && validReviewCommentLine(comment.OriginalLine) && validReviewCommentLine(comment.OriginalStartLine) && (comment.Side == "" || comment.Side == "LEFT" || comment.Side == "RIGHT") && (comment.StartSide == "" || comment.StartSide == "LEFT" || comment.StartSide == "RIGHT")
}

func validIdentifierText(value string, max int) bool {
	if value == "" || len(value) > max || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r == 0x7f {
			return false
		}
	}
	return true
}

func validBody(value string) bool {
	if len(value) > 4*githubMaxText || !utf8.ValidString(value) || utf8.RuneCountInString(value) > githubMaxText {
		return false
	}
	for _, r := range value {
		if (r < 0x20 && r != '\t' && r != '\r' && r != '\n') || r == 0x7f {
			return false
		}
	}
	return true
}

func validPlainText(value string, max int) bool {
	if len(value) > max || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if (r < 0x20 && r != '\t' && r != '\r' && r != '\n') || r == 0x7f {
			return false
		}
	}
	return true
}

func validSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] >= '0' && value[i] <= '9' || value[i] >= 'a' && value[i] <= 'f' || value[i] >= 'A' && value[i] <= 'F' {
			continue
		}
		return false
	}
	return true
}

func validRepoPath(value string) bool {
	if value == "" || len(value) > 1024 || !utf8.ValidString(value) || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return validPlainText(value, 1024)
}

func parseTimestamp(value string) (time.Time, error) {
	if value == "" || len(value) > 64 {
		return time.Time{}, errors.New("invalid timestamp")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed, nil
}
