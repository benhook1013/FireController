package reviewprovider

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const ciFailureLogLimit = 1 << 20

var (
	actionsLogHost = regexp.MustCompile(`^productionresultssa[0-9]+\.blob\.core\.windows\.net$`)
	ansiCIColor    = regexp.MustCompile("\\x1b\\[[0-9;]*m")
	ciGoFailure    = regexp.MustCompile(`^--- FAIL: (Test[A-Za-z0-9_]{1,96})(?:/[A-Za-z0-9_./-]+)? \([0-9.]+s\)$`)
	ciJVMFailure   = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_.$]{1,96}) > ([A-Za-z_][A-Za-z0-9_]{0,63})\(\) FAILED$`)
)

// CIFailedTestNames accepts only the exact failed Actions job selected by the
// observation caller. Returned names are diagnostic; absence is not a verdict.
func (g *GitHub) CIFailedTestNames(ctx context.Context, repository string, jobID int64) []string {
	if ctx == nil || g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) || !validRepositorySelector(repository) || jobID <= 0 {
		return nil
	}
	path := "/repos/" + repository + "/actions/jobs/" + strconv.FormatInt(jobID, 10) + "/logs"
	apiClient := &http.Client{Transport: g.httpClient.Transport, Timeout: githubRequestLimit, CheckRedirect: refuseCILogRedirect}
	_, headers, status, err := g.requestBytesWithClient(ctx, apiClient, http.MethodGet, path, nil, nil)
	if err != nil || status != http.StatusFound || len(headers.Values("Location")) != 1 {
		return nil
	}
	loc, err := url.Parse(headers.Get("Location"))
	if err != nil || loc.Scheme != "https" || !actionsLogHost.MatchString(loc.Host) || loc.User != nil || loc.Fragment != "" || loc.Opaque != "" || loc.RawQuery == "" || loc.Path == "" {
		return nil
	}
	dataRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, loc.String(), nil)
	if err != nil {
		return nil
	}
	response, err := apiClient.Do(dataRequest)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > ciFailureLogLimit {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, ciFailureLogLimit+1))
	if err != nil || len(data) > ciFailureLogLimit {
		return nil
	}
	return parseCIFailedTests(data)
}

func refuseCILogRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

func parseCIFailedTests(data []byte) []string {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 16<<10)
	seen := map[string]bool{}
	var names []string
	for count := 0; count < 16384 && scanner.Scan(); count++ {
		line := strings.TrimSpace(ansiCIColor.ReplaceAllString(scanner.Text(), ""))
		// Actions prefixes each runner log line with its RFC3339 timestamp.
		if prefix, rest, ok := strings.Cut(line, " "); ok {
			if _, err := time.Parse(time.RFC3339Nano, prefix); err == nil {
				line = strings.TrimSpace(rest)
			}
		}
		name := ""
		if m := ciGoFailure.FindStringSubmatch(line); m != nil {
			name = m[1]
		} else if m := ciJVMFailure.FindStringSubmatch(line); m != nil {
			name = m[1] + "." + m[2]
		}
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
			if len(names) == 8 {
				break
			}
		}
	}
	return names
}
