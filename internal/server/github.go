package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func validRepository(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || len(parts[0]) < 1 || len(parts[0]) > 39 || len(parts[1]) < 1 || len(parts[1]) > 100 || parts[1] == "." || parts[1] == ".." {
		return false
	}
	for i, part := range parts {
		for _, c := range part {
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || i == 1 && (c == '.' || c == '_') {
				continue
			}
			return false
		}
	}
	return true
}

// GitHubResolver loads optional operator credentials once and verifies only the
// explicitly requested repository. It never starts a queue scan or mutation.
func GitHubResolver(tokenFile string) (RepositoryResolver, error) {
	token := ""
	if tokenFile != "" {
		info, err := os.Lstat(tokenFile)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
			return nil, core.Fail("configuration_invalid", "The GitHub credential must be a private regular file.", 500)
		}
		f, err := os.Open(tokenFile)
		if err != nil {
			return nil, core.Fail("configuration_invalid", "The GitHub credential file is unavailable.", 500)
		}
		opened, err := f.Stat()
		if err != nil || !opened.Mode().IsRegular() {
			_ = f.Close()
			return nil, core.Fail("configuration_invalid", "The GitHub credential must be a private regular file.", 500)
		}
		data, err := io.ReadAll(io.LimitReader(f, 4097))
		closeErr := f.Close()
		token = strings.TrimSpace(string(data))
		if err != nil || closeErr != nil || len(data) > 4096 || token == "" || strings.ContainsAny(token, " \t\r\n") {
			return nil, core.Fail("configuration_invalid", "The GitHub credential file is invalid.", 500)
		}
	}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 || req.URL.Scheme != "https" || req.URL.Host != "api.github.com" {
			return errors.New("unsupported provider redirect")
		}
		return nil
	}}
	return func(ctx context.Context, repository string) (RepositoryIdentity, error) {
		if !validRepository(repository) {
			return RepositoryIdentity{}, core.Fail("validation_failed", "Use an owner/repository GitHub reference.", 400)
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		parts := strings.Split(repository, "/")
		req, err := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/"+url.PathEscape(parts[0])+"/"+url.PathEscape(parts[1]), nil)
		if err != nil {
			return RepositoryIdentity{}, core.Fail("provider_unavailable", "Repository verification is unavailable.", 503)
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "FireController")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(req)
		if err != nil {
			return RepositoryIdentity{}, core.Fail("provider_unavailable", "GitHub repository verification failed.", 503)
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return RepositoryIdentity{}, core.Fail("provider_unavailable", "GitHub repository verification failed.", 503)
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		if err != nil || len(data) > 1<<20 {
			return RepositoryIdentity{}, core.Fail("provider_unavailable", "GitHub returned an unavailable or oversized repository response.", 503)
		}
		var result struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		}
		if json.Unmarshal(data, &result) != nil || result.ID <= 0 || !validRepository(result.FullName) {
			return RepositoryIdentity{}, core.Fail("provider_unavailable", "GitHub returned an invalid repository identity.", 503)
		}
		return RepositoryIdentity{result.ID, result.FullName}, nil
	}, nil
}
