package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/apiroutes"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/historycollect"
	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestDaemonRouteIndex(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "private")
	store, err := core.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := ensureDaemonSchemas(ctx, store); err != nil {
		t.Fatal(err)
	}
	adminBytes, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	admin := strings.TrimSpace(string(adminBytes))
	const token = "synthetic-route-index-reader-token-000000"
	sum := sha256.Sum256([]byte(token))
	for _, statement := range []string{
		`INSERT INTO projects(id,name,github_repository_id,repository) VALUES('index-project','Private project name',1,'private/repository')`,
		`INSERT INTO credentials(id,principal_id,token_sha256,is_admin,revoked) VALUES('index-credential','private-principal','` + hex.EncodeToString(sum[:]) + `',0,0)`,
		`INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES('index-credential','index-project',1,0)`,
	} {
		if _, err := store.DB().ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	execution, err := reviewexec.New(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	collection, err := historycollect.New(store, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	// The real composition mounts disabled optional history routes, all private
	// controls, and (in the second case) an installed provider hook. No workers run.
	for _, installedHook := range []bool{false, true} {
		t.Run(map[bool]string{false: "without hook", true: "with hook"}[installedHook], func(t *testing.T) {
			var hook http.Handler
			if installedHook {
				hook = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(418) })
			}
			mux := newDaemonRoutes(ctx, store, execution, collection, hook)
			request := func(method, path, credential, instance string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, path, nil)
				if credential != "" {
					r.Header.Set("Authorization", "Bearer "+credential)
				}
				if instance != "" {
					r.Header.Set("X-FC-Instance", instance)
				}
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)
				return w
			}
			for _, credential := range []string{"", "invalid-synthetic-token"} {
				w := request("GET", "/api/v1", credential, "")
				if w.Code != 401 || strings.Contains(w.Body.String(), `"routes"`) {
					t.Fatalf("unauthenticated catalogue: %d %s", w.Code, w.Body.String())
				}
			}
			w := request("GET", "/api/v1", token, store.InstanceID())
			if w.Code != 200 {
				t.Fatalf("catalogue: %d %s", w.Code, w.Body.String())
			}
			var index apiroutes.Index
			if err := json.Unmarshal(w.Body.Bytes(), &index); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(index, mux.Index()) || index.MethodScope == "" {
				t.Fatal("wire/recorder parity")
			}
			if got := request("GET", "/api/v1", admin, ""); got.Code != 200 || got.Body.String() != w.Body.String() {
				t.Fatal("catalogue depends on caller privileges")
			}
			for _, sensitive := range []string{token, admin, "index-project", "private-principal", "private/repository", "Private project name", store.InstanceID()} {
				if strings.Contains(w.Body.String(), sensitive) {
					t.Fatal("catalogue contains instance data")
				}
			}
			present := map[apiroutes.Route]bool{}
			for i, route := range index.Routes {
				if present[route] {
					t.Fatalf("duplicate route: %v", route)
				}
				present[route] = true
				if i > 0 {
					prev := index.Routes[i-1]
					if prev.Path > route.Path || prev.Path == route.Path && prev.Method >= route.Method {
						t.Fatal("nondeterministic order")
					}
				}
				if !strings.HasPrefix(route.Path, "/api/v1") || route.Method == "HEAD" {
					t.Fatalf("invalid scope: %v", route)
				}
				for _, hidden := range []string{"browser-session", "/credentials", "/operator/", "/hooks/", "/import", "/rounds/adoption/", "/adoption-obligations/preview", "/adoption-obligations/apply", "retained-recovery", "/history/collections"} {
					if strings.Contains(route.Path, hidden) {
						t.Fatalf("private route advertised: %v", route)
					}
				}
			}
			for _, route := range []apiroutes.Route{
				{Method: "GET", Path: "/api/v1"}, {Method: "GET", Path: "/api/v1/instance"},
				{Method: "GET", Path: "/api/v1/projects/{project_id}/jobs"}, {Method: "POST", Path: "/api/v1/projects/{project_id}/jobs"},
				{Method: "GET", Path: "/api/v1/projects/{project_id}/vars"}, {Method: "GET", Path: "/api/v1/projects/{project_id}/vars/{key}/history"},
				{Method: "GET", Path: "/api/v1/projects/{project_id}/presentation"},
				{Method: "POST", Path: "/api/v1/projects/{project_id}/reviews/requests"},
				{Method: "GET", Path: "/api/v1/projects/{project_id}/reviews/pulls/{number}"},
				{Method: "PUT", Path: "/api/v1/projects/{project_id}/reviews/pulls/{pull_number}/following"},
				{Method: "GET", Path: "/api/v1/projects/{project_id}/reviews/adoption-obligations"},
				{Method: "POST", Path: "/api/v1/projects/{project_id}/reviews/adoption-obligations/{id}/resolve"},
				{Method: "GET", Path: "/api/v1/public/project-locations/{address}"},
				{Method: "GET", Path: "/api/v1/public/projects/{slug}"}, {Method: "GET", Path: "/api/v1/public/projects/{slug}/events"},
			} {
				if !present[route] {
					t.Errorf("supported route missing: %v", route)
				}
			}
			for _, route := range []apiroutes.Route{
				{Method: "GET", Path: "/api/v1/projects"}, {Method: "POST", Path: "/api/v1/projects"},
				{Method: "PUT", Path: "/api/v1/projects/{project_id}/vars/{key}"},
				{Method: "PUT", Path: "/api/v1/projects/{project_id}/presentation"},
				{Method: "POST", Path: "/api/v1/projects/{project_id}/reviews/scheduling"},
				{Method: "POST", Path: "/api/v1/projects/{project_id}/reviews/policy"},
				{Method: "POST", Path: "/api/v1/projects/{project_id}/reviews/queue-sorting"},
				{Method: "POST", Path: "/api/v1/projects/{project_id}/reviews/attempts/{attempt_id}/settlement"},
			} {
				if present[route] {
					t.Errorf("administrative route advertised: %v", route)
				}
			}
			for _, tc := range []struct {
				method, path, credential, instance string
				status                             int
			}{
				{"GET", "/api/v1", token, "another-instance", 409},
				{"POST", "/api/v1", token, "", 404}, // Preserve the base server's catch-all response.
				{"GET", "/api/v1/", token, "", 404},
				{"GET", "/api/v1/unknown", token, "", 404},
				{"GET", "/api/v1/projects/index-project/jobs", token, "", 200},
				{"POST", "/api/v1/projects/index-project/jobs", token, "", 403},
				{"GET", "/api/v1/projects", token, "", 403},
				{"GET", "/api/v1/projects/index-project/browser-session", "", "", 401},
				{"POST", "/api/v1/projects/index-project/history/collections", token, "", 403},
				{"POST", "/api/v1/projects/index-project/reviews/attempts/synthetic/retained-recovery", token, "", 403},
				{"GET", "/api/v1/operator/deployment-drain", token, "", 403},
				{"GET", "/api/v1/public/projects/not-published", token, "", 404},
				{"GET", "/api/v1/public/projects/not-published", "", "", 404},
			} {
				got := request(tc.method, tc.path, tc.credential, tc.instance)
				if got.Code != tc.status {
					t.Errorf("%s %s = %d want %d: %s", tc.method, tc.path, got.Code, tc.status, got.Body.String())
				}
			}
			if installedHook && request("POST", "/api/v1/hooks/github", "", "").Code != 418 {
				t.Fatal("private hook no longer mounted")
			}
			for _, headers := range []http.Header{
				{"Authorization": {"Bearer " + token, "Bearer " + token}},
				{"Cookie": {"fc-local-browser-synthetic=not-a-session"}},
				{"Authorization": {"Bearer " + token}, "X-Fc-Browser-Session": {"synthetic"}},
			} {
				r := httptest.NewRequest("GET", "/api/v1", nil)
				r.Header = headers
				got := httptest.NewRecorder()
				mux.ServeHTTP(got, r)
				if got.Code != 401 {
					t.Errorf("invalid authentication accepted: %d", got.Code)
				}
			}
		})
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE credentials SET revoked=1 WHERE id='index-credential'`); err != nil {
		t.Fatal(err)
	}
	mux := newDaemonRoutes(ctx, store, execution, collection, nil)
	r := httptest.NewRequest("GET", "/api/v1", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("revoked credential: %d", w.Code)
	}
}
