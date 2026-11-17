package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestGitHubCredentialRejectsUnsafeHeaderBytes(t *testing.T) {
	for name, token := range map[string]string{
		"nul": "synthetic\x00secret", "control": "synthetic\x01secret",
		"delete": "synthetic\x7fsecret", "unicode": "syntheticΩsecret",
		"invalid UTF8": "synthetic\xffsecret", "space": "synthetic secret",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credential")
			if err := os.WriteFile(path, []byte(token), 0600); err != nil {
				t.Fatal(err)
			}
			loaded, err := loadGitHubCredential(path)
			var failure *core.Error
			if loaded != "" || !errors.As(err, &failure) || failure.Code != "configuration_invalid" || strings.Contains(err.Error(), "synthetic") || strings.Contains(err.Error(), path) {
				t.Fatal("unsafe credential accepted or disclosed")
			}
			if resolver, err := GitHubResolver(path); resolver != nil || err == nil {
				t.Fatal("public resolver accepted unsafe credential")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("synthetic-token!~\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if token, err := loadGitHubCredential(path); err != nil || token != "synthetic-token!~" {
		t.Fatal("printable token with file newline rejected")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestGitHubResolverClassifiesRepositoryNotFoundAndHidesProviderBody(t *testing.T) {
	const privateDetail = "provider-body-must-not-be-returned"
	var requestedPath, authorization string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestedPath = request.URL.Path
		authorization = request.Header.Get("Authorization")
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(`{"message":"` + privateDetail + `"}`)), Header: make(http.Header), Request: request}, nil
	})}
	identity, err := githubResolver("synthetic-operator-token", client)(context.Background(), "owner/repository")
	if identity != (RepositoryIdentity{}) {
		t.Fatalf("not-found identity: %#v", identity)
	}
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != "repository_not_found" || failure.Status != http.StatusNotFound {
		t.Fatalf("not-found classification: %#v", err)
	}
	if strings.Contains(err.Error(), privateDetail) || strings.Contains(err.Error(), "synthetic-operator-token") {
		t.Fatal("resolver exposed provider content or credential")
	}
	if requestedPath != "/repos/owner/repository" || authorization != "Bearer synthetic-operator-token" {
		t.Fatalf("request path/auth = %q / %q", requestedPath, authorization)
	}
}

func TestGitHubResolverKeepsRateLimitAndOtherProviderFailuresUnavailable(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("private provider response")), Header: make(http.Header), Request: request}, nil
			})}
			_, err := githubResolver("", client)(context.Background(), "owner/repository")
			var failure *core.Error
			if !errors.As(err, &failure) || failure.Code != "provider_unavailable" || failure.Status != http.StatusServiceUnavailable {
				t.Fatalf("provider status %d classification: %#v", status, err)
			}
			if strings.Contains(err.Error(), "private provider response") {
				t.Fatal("resolver exposed provider response")
			}
		})
	}
}
