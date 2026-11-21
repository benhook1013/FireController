package apiroutes

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestRegistrationParity(t *testing.T) {
	r := New()
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Matched-ID", req.PathValue("job_id"))
		w.WriteHeader(204)
	})
	Client(r).Handle("POST /api/v1/projects/{project_id}/jobs", h)
	Public(r).HandleFunc("GET /api/v1/public/projects/{slug}", h)
	Client(r).Handle("GET /api/v1/projects/{project_id}/jobs/{job_id}", h)
	Private(r).Handle("DELETE /api/v1/private/{job_id}", h)
	r.Handle("GET /healthz", h)
	// The catalogue order is path then method, independently of registration order.
	want := []Route{{"POST", "/api/v1/projects/{project_id}/jobs"}, {"GET", "/api/v1/projects/{project_id}/jobs/{job_id}"}, {"GET", "/api/v1/public/projects/{slug}"}}
	if !reflect.DeepEqual(r.Index().Routes, want) {
		t.Fatalf("index = %#v", r.Index())
	}
	for _, route := range r.Index().Routes {
		path := strings.NewReplacer("{project_id}", "synthetic-project", "{job_id}", "synthetic-job", "{slug}", "synthetic-public").Replace(route.Path)
		req := httptest.NewRequest(route.Method, path, nil)
		_, pattern := r.mux.Handler(req)
		if pattern != route.Method+" "+route.Path {
			t.Fatalf("advertised %v matched %q", route, pattern)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 204 {
			t.Fatalf("%v: %d", route, w.Code)
		}
		if strings.Contains(route.Path, "{job_id}") && w.Header().Get("Matched-ID") != "synthetic-job" {
			t.Fatal("path values changed")
		}
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"DELETE", "/api/v1/private/job", 204},
		{"GET", "/healthz", 204},
		{"PUT", "/api/v1/projects/project/jobs", 405},
		{"GET", "/api/v1/projects/project/jobs/job/extra", 404},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status {
			t.Fatalf("%s %s = %d", tc.method, tc.path, w.Code)
		}
	}
	copy := r.Index()
	copy.Routes[0].Path = "changed"
	if !reflect.DeepEqual(r.Index().Routes, want) {
		t.Fatal("caller changed recorder metadata")
	}
}

func TestClassificationFailsClosed(t *testing.T) {
	for _, pattern := range []string{"GET /api/v1", "GET /api/v1/new", "/api/v1/new", "GET\t/api/v1/new", "GET synthetic.example/api/v1/new", "synthetic.example/api/v1/new"} {
		t.Run(pattern, func(t *testing.T) {
			r := New()
			defer func() {
				if recover() == nil {
					t.Fatal("unclassified v1 route accepted")
				}
				if len(r.Index().Routes) != 0 {
					t.Fatal("failed registration advertised")
				}
			}()
			r.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
		})
	}
	for _, pattern := range []string{"/api/v1/new", "GET /healthz", "GET /api/v1/new/", "GET /api/v1/new/{broken"} {
		t.Run("invalid "+pattern, func(t *testing.T) {
			r := New()
			defer func() {
				if recover() == nil {
					t.Fatal("invalid advertisement accepted")
				}
				if len(r.Index().Routes) != 0 {
					t.Fatal("stale advertisement")
				}
			}()
			Client(r).HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
		})
	}
	r := New()
	Client(r).HandleFunc("GET /api/v1/new", func(http.ResponseWriter, *http.Request) {})
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("duplicate accepted")
			}
		}()
		Client(r).HandleFunc("GET /api/v1/new", func(http.ResponseWriter, *http.Request) {})
	}()
	if len(r.Index().Routes) != 1 {
		t.Fatal("duplicate left stale advertisement")
	}
}
