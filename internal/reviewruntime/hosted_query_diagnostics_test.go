package reviewruntime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestHostedQueryDiagnosticsNativeScopedOptInRead(t *testing.T) {
	f := newHostedDriverFixture(t)
	f.provider.reply = "Unsupported synthetic provider reply. PRIVATE_CAPTURE_BODY"
	op, attempt, config := f.startHosted(t)
	if _, err := f.hosted.Prepare(t.Context(), op, config, attempt); err == nil {
		t.Fatal("unsupported allowance became permission")
	}
	if _, err := f.engine.Advance(t.Context(), f.project, attempt.ID, attempt.Revision, "held", "hosted_allowance_unproven", ""); err != nil {
		t.Fatal(err)
	}
	query, err := f.engine.ReadHostedAllowanceQuery(t.Context(), f.project, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	observations, err := f.engine.ReadHostedObservations(t.Context(), f.project, attempt.ID)
	if err != nil || len(observations) != 1 {
		t.Fatal("retained query evidence missing", err)
	}
	want := reviewexec.HostedQueryDiagnostic{QueryID: query.ID, QueryCommentID: 100, QueryURL: "https://github.com/owner/repo/pull/2#issuecomment-100", ObservationID: observations[0].ID, CaptureID: observations[0].Input.CaptureID, CapturedAt: observations[0].Input.ObservedAt}
	mux := http.NewServeMux()
	f.engine.Register(mux)
	if _, err := f.store.DB().Exec(`INSERT INTO projects(id,name,github_repository_id,repository) VALUES('different-project','Different project',902,'other/repo')`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, project, token, header string
		status                       int
		details                      bool
	}{
		{"explicit", f.project, f.readerToken(), "1", 200, true},
		{"legacy", f.project, f.readerToken(), "", 200, false},
		{"unsupported opt-in", f.project, f.readerToken(), "true", 200, false},
		{"anonymous", f.project, "", "1", 401, false},
		{"foreign project", "different-project", f.readerToken(), "1", 403, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/v1/projects/"+tc.project+"/reviews/operations/"+op.ID, nil)
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			r.Header.Set("X-FC-Hosted-Query", tc.header)
			r.Header.Set("X-FC-Operation-Evidence", "true")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != tc.status || strings.Contains(w.Body.String(), "PRIVATE_CAPTURE_BODY") || strings.Contains(w.Body.String(), f.capability.GitHubCredential()) || strings.Contains(w.Body.String(), "@coderabbitai") {
				t.Fatal("query diagnostics disclosed raw source or changed access", w.Code, w.Body.String())
			}
			if tc.status == 200 {
				var got reviewexec.Operation
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if tc.details && (got.HostedQuery == nil || *got.HostedQuery != want) || !tc.details && got.HostedQuery != nil {
					t.Fatal("original handles changed or legacy shape expanded", w.Body.String())
				}
			} else if strings.Contains(w.Body.String(), query.ID) || strings.Contains(w.Body.String(), observations[0].Input.CaptureID) {
				t.Fatal("query source handles escaped project authorization")
			}
		})
	}
	if f.provider.queries != 1 || f.provider.reviews != 0 {
		t.Fatal("diagnostic read did provider work")
	}
	compact := httptest.NewRequest("GET", "/api/v1/projects/"+f.project+"/reviews/operations/"+op.ID+"/status", nil)
	compact.Header.Set("Authorization", "Bearer "+f.readerToken())
	compact.Header.Set("X-FC-Hosted-Query", "1")
	compactResponse := httptest.NewRecorder()
	mux.ServeHTTP(compactResponse, compact)
	if compactResponse.Code != 200 || strings.Contains(compactResponse.Body.String(), "hosted_query") || strings.Contains(compactResponse.Body.String(), query.ID) {
		t.Fatal("ordinary compact output expanded into query diagnostics", compactResponse.Code, compactResponse.Body.String())
	}
	if _, err := f.store.DB().Exec(`UPDATE review_hosted_observations SET body_json=? WHERE attempt_id=?`, `{"private":"PRIVATE_CORRUPT_BODY"}`, attempt.ID); err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"", "1"} {
		r := httptest.NewRequest("GET", "/api/v1/projects/"+f.project+"/reviews/operations/"+op.ID, nil)
		r.Header.Set("Authorization", "Bearer "+f.readerToken())
		r.Header.Set("X-FC-Hosted-Query", header)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		wantCode := 200
		if header == "1" {
			wantCode = 500
		}
		if w.Code != wantCode || strings.Contains(w.Body.String(), "PRIVATE_CORRUPT_BODY") || strings.Contains(w.Body.String(), "hosted_query") {
			t.Fatal("corrupt selected diagnostic did not fail closed or changed legacy reads", w.Code, w.Body.String())
		}
	}
}

func TestHostedQueryDiagnosticsMissingOriginalQueryNotInvented(t *testing.T) {
	f := newHostedDriverFixture(t)
	op, attempt, _ := f.startHosted(t)
	if _, err := f.engine.Advance(t.Context(), f.project, attempt.ID, attempt.Revision, "held", "hosted_allowance_unproven", ""); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.engine.Register(mux)
	r := httptest.NewRequest("GET", "/api/v1/projects/"+f.project+"/reviews/operations/"+op.ID, nil)
	r.Header.Set("Authorization", "Bearer "+f.readerToken())
	r.Header.Set("X-FC-Hosted-Query", "1")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "hosted_query") || f.provider.queries != 0 || f.provider.reviews != 0 {
		t.Fatal("missing retained query invented source", w.Code, w.Body.String())
	}
}
