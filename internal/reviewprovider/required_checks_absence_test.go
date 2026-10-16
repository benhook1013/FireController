package reviewprovider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequiredCheckAuthorityUnprotectedBranchProvesClassicAbsence(t *testing.T) {
	for _, branch := range []string{"main", "codex/topic-one"} {
		t.Run(branch, func(t *testing.T) {
			var paths []string
			selected := "/repos/owner/repo/branches/" + url.PathEscape(branch)
			rulesPath := "/repos/owner/repo/rules/branches/" + url.PathEscape(branch)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.EscapedPath())
				if r.Method != http.MethodGet || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" || r.Header.Get("Authorization") != "Bearer synthetic-authority-token" {
					t.Error("absence fallback changed method, pinned version or credential")
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.EscapedPath() {
				case selected + "/protection":
					w.WriteHeader(http.StatusNotFound)
					_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
				case selected:
					if r.URL.RawQuery != "" {
						t.Error("fallback read an unselected branch query")
					}
					_, _ = fmt.Fprintf(w, `{"name":%q,"protected":false,"commit":{"sha":"unused-provider-metadata"}}`, branch)
				case rulesPath:
					_, _ = fmt.Fprint(w, `[]`)
				default:
					t.Error("absence fallback read unrelated scope", r.URL.EscapedPath())
					w.WriteHeader(500)
				}
			}))
			defer s.Close()
			result, err := testGitHub(s, "synthetic-authority-token").RequiredCheckAuthority(t.Context(), "owner/repo", branch)
			if err != nil || result.State != "complete" || result.Reason != "" || result.Checks == nil || len(result.Checks) != 0 || result.Strict || result.Repository != "owner/repo" || result.Branch != branch || result.ObservedAt.IsZero() || !reflect.DeepEqual(paths, []string{selected + "/protection", selected, rulesPath}) {
				t.Fatalf("scoped existing unprotected branch did not prove empty classic authority: result=%+v err=%v paths=%v", result, err, paths)
			}
		})
	}
}

func TestRequiredCheckAuthorityAbsenceStillRequiresCompleteApplicableRules(t *testing.T) {
	for _, failSecond := range []bool{false, true} {
		t.Run(fmt.Sprintf("later_page_failure_%t", failSecond), func(t *testing.T) {
			var calls atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet {
					t.Error("authority performed a mutation")
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.EscapedPath() {
				case "/repos/owner/repo/branches/codex%2Ftopic/protection":
					w.WriteHeader(404)
				case "/repos/owner/repo/branches/codex%2Ftopic":
					_, _ = fmt.Fprint(w, `{"name":"codex/topic","protected":false}`)
				case "/repos/owner/repo/rules/branches/codex%2Ftopic":
					if r.URL.Query().Get("page") == "2" {
						if failSecond {
							w.WriteHeader(403)
							return
						}
						_, _ = fmt.Fprint(w, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"lint"}],"strict_required_status_checks_policy":false}}]`)
						return
					}
					w.Header().Set("Link", "<"+serverURL(r)+`/repos/owner/repo/rules/branches/codex%2Ftopic?per_page=100&page=2>; rel="next"`)
					_, _ = fmt.Fprint(w, "["+authorityRule+"]")
				default:
					t.Error("unselected route", r.URL.EscapedPath())
					w.WriteHeader(500)
				}
			}))
			defer s.Close()
			result, err := testGitHub(s, "").RequiredCheckAuthority(t.Context(), "owner/repo", "codex/topic")
			if calls.Load() != 4 {
				t.Fatal("did not read exactly classic, selected branch and two rule pages", calls.Load())
			}
			if failSecond {
				if err == nil || result.State != "unproven" || result.Checks != nil {
					t.Fatal("partial rules became complete absence", result, err)
				}
			} else if err != nil || result.State != "complete" || !result.Strict || !reflect.DeepEqual(result.Checks, []RequiredCheck{{Context: "build", AppID: 17}, {Context: "lint"}}) {
				t.Fatal("fallback lost applicable strictness or app binding", result, err)
			}
		})
	}
}

func TestRequiredCheckAuthorityProtectedBranchLeavesClassicAbsenceUnproven(t *testing.T) {
	for _, branch := range []string{"main", "codex/topic-one"} {
		for _, summary := range []string{"", `,"protection":{"enabled":false,"required_status_checks":{"contexts":[],"checks":[]}}`} {
			t.Run(branch+summary, func(t *testing.T) {
				selected := "/repos/owner/repo/branches/" + url.PathEscape(branch)
				var paths []string
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					paths = append(paths, r.URL.EscapedPath())
					if r.Method != http.MethodGet || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer synthetic-authority-token" {
						t.Error("ambiguous classic absence changed request scope")
					}
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.EscapedPath() {
					case selected + "/protection":
						w.WriteHeader(http.StatusNotFound)
						_, _ = fmt.Fprint(w, `{"message":"PRIVATE synthetic-authority-token"}`)
					case selected:
						_, _ = fmt.Fprintf(w, `{"name":%q,"protected":true,"private_metadata":"PRIVATE synthetic-authority-token"%s}`, branch, summary)
					default:
						t.Error("ambiguous classic absence read rules or unrelated scope", r.URL.EscapedPath())
						w.WriteHeader(http.StatusInternalServerError)
					}
				}))
				defer s.Close()
				result, err := testGitHub(s, "synthetic-authority-token").RequiredCheckAuthority(t.Context(), "owner/repo", branch)
				if err != nil || result.State != "unproven" || result.Reason != "classic_absence_unproven_ruleset_branch" || result.ObservedAt.IsZero() || result.Strict || result.Checks != nil || result.Repository != "owner/repo" || result.Branch != branch || !reflect.DeepEqual(paths, []string{selected + "/protection", selected}) {
					t.Fatalf("protected summary did not retain explicit ambiguous classic absence: result=%+v err=%v paths=%v", result, err, paths)
				}
				if exposed := fmt.Sprint(result, err); strings.Contains(exposed, "PRIVATE") || strings.Contains(exposed, "synthetic-authority-token") {
					t.Fatal("ambiguous classic absence exposed private provider text")
				}
			})
		}
	}
}

func TestRequiredCheckAuthorityAbsenceRefusesUnknownBranchProtection(t *testing.T) {
	for name, body := range map[string]string{
		"wrong_protected_name": `{"name":"other","protected":true}`,
		"absent_protected":     `{"name":"main"}`,
		"null_protected":       `{"name":"main","protected":null}`,
		"wrong_type":           `{"name":"main","protected":"false"}`,
		"wrong_name":           `{"name":"other","protected":false}`,
		"case_changed_name":    `{"name":"Main","protected":false}`,
		"absent_name":          `{"protected":false}`,
		"null_name":            `{"name":null,"protected":false}`,
		"duplicate_protected":  `{"name":"main","protected":true,"protected":false}`,
		"alias_protected":      `{"name":"main","Protected":false}`,
		"invalid_json":         `{"name":"main",`,
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/repos/owner/repo/branches/main/protection" {
					w.WriteHeader(404)
					return
				}
				if r.URL.Path != "/repos/owner/repo/branches/main" {
					t.Error("unknown branch still read rules", r.URL.Path)
				}
				_, _ = fmt.Fprint(w, body)
			}))
			defer s.Close()
			result, err := testGitHub(s, "").RequiredCheckAuthority(t.Context(), "owner/repo", "main")
			if err == nil || result.State != "unproven" || result.Checks != nil || calls.Load() != 2 {
				t.Fatal("unknown classic authority became proven absence", result, err, calls.Load())
			}
		})
	}
}

func TestRequiredCheckAuthorityAbsenceFallbackOnlyOnClassic404(t *testing.T) {
	for _, tc := range []struct{ classic, branch, calls int }{{403, 200, 1}, {429, 200, 1}, {401, 200, 1}, {500, 200, 1}, {302, 200, 1}, {404, 404, 2}, {404, 403, 2}, {404, 429, 2}, {404, 302, 2}} {
		t.Run(fmt.Sprintf("%d-%d", tc.classic, tc.branch), func(t *testing.T) {
			var calls atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Location", "/unexpected-redirect")
				status := tc.branch
				if r.URL.Path == "/repos/owner/repo/branches/main/protection" {
					status = tc.classic
				} else if r.URL.Path != "/repos/owner/repo/branches/main" {
					t.Error("fallback followed redirect or read unrelated route", r.URL.Path)
				}
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, `{"message":"Branch not protected PRIVATE synthetic-authority-token"}`)
			}))
			defer s.Close()
			result, err := testGitHub(s, "synthetic-authority-token").RequiredCheckAuthority(t.Context(), "owner/repo", "main")
			if err == nil || result.State != "unproven" || result.Checks != nil || int(calls.Load()) != tc.calls || strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), "synthetic-authority-token") {
				t.Fatal("failure was bypassed/retried/leaked", result, err, calls.Load())
			}
		})
	}
}

func TestRequiredCheckAuthorityAbsencePreservesCallerBudgetAndCancellation(t *testing.T) {
	var calls atomic.Int64
	branchStarted := make(chan struct{})
	branchClosed := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/protection") {
			w.WriteHeader(404)
			return
		}
		close(branchStarted)
		defer close(branchClosed)
		<-r.Context().Done()
	}))
	defer s.Close()
	g := testGitHub(s, "")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := g.RequiredCheckAuthority(ctx, "owner/repo", "main"); done <- err }()
	select {
	case <-branchStarted:
	case <-time.After(time.Second):
		t.Fatal("bounded branch fallback did not start")
	}
	select {
	case err := <-done:
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "provider_unavailable" {
			t.Fatal("caller deadline was lost", err)
		}
	case <-time.After(time.Second):
		t.Fatal("branch fallback reset the caller budget")
	}
	select {
	case <-branchClosed:
	case <-time.After(time.Second):
		t.Fatal("canceled fallback retained its request")
	}
	if calls.Load() != 2 {
		t.Fatal("canceled fallback retried or read rules")
	}
}
