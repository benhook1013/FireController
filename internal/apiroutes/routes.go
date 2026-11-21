// Package apiroutes records discovery metadata at the point a route is mounted.
// Classification affects metadata only, never routing or handler authorization.
package apiroutes

import (
	"net/http"
	"sort"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

// Registrar is implemented by both ServeMux and the production Recorder.
type Registrar interface {
	Handle(string, http.Handler)
	HandleFunc(string, func(http.ResponseWriter, *http.Request))
}

type classification string

const (
	client  classification = "client"
	public  classification = "public"
	private classification = "private"
)

// Client opts a supported scoped client operation into discovery.
func Client(mux Registrar) Registrar { return classified{mux, client} }

// Public opts an anonymous public-site operation into discovery.
func Public(mux Registrar) Registrar { return classified{mux, public} }

// Private explicitly excludes administrative, internal and compatibility routes.
func Private(mux Registrar) Registrar { return classified{mux, private} }

type classified struct {
	mux  Registrar
	kind classification
}

func (c classified) Handle(pattern string, handler http.Handler) {
	if recorder, ok := c.mux.(*Recorder); ok {
		recorder.register(pattern, handler, c.kind)
		return
	}
	c.mux.Handle(pattern, handler)
}

func (c classified) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	if handler == nil {
		panic("apiroutes: nil handler")
	}
	c.Handle(pattern, http.HandlerFunc(handler))
}

// Route contains only the explicitly registered method and literal path template.
type Route struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// Index is static instance capability metadata, not a caller's grants or data.
type Index struct {
	Routes      []Route `json:"routes"`
	MethodScope string  `json:"method_scope"`
}

// Recorder owns its mux so production registrations cannot silently bypass it.
// Compose routes before serving; as with ServeMux, duplicate patterns panic.
type Recorder struct {
	mux    *http.ServeMux
	routes []Route
}

func New() *Recorder { return &Recorder{mux: http.NewServeMux()} }

func (r *Recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) { r.mux.ServeHTTP(w, req) }

func (r *Recorder) Handle(pattern string, handler http.Handler) { r.register(pattern, handler, "") }
func (r *Recorder) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	if handler == nil {
		panic("apiroutes: nil handler")
	}
	r.Handle(pattern, http.HandlerFunc(handler))
}

func (r *Recorder) register(pattern string, handler http.Handler, kind classification) {
	// ServeMux permits spaces or tabs after the method and an optional host.
	// Detect v1 registrations in all those forms rather than allowing alternate
	// pattern spellings to bypass the classification guard.
	method, path, host := "", pattern, ""
	explicit := false
	if i := strings.IndexAny(pattern, " \t"); i >= 0 {
		method, path, explicit = pattern[:i], strings.TrimLeft(pattern[i+1:], " \t"), true
	}
	if i := strings.IndexByte(path, '/'); i >= 0 {
		host, path = path[:i], path[i:]
	}
	v1 := path == "/api/v1" || strings.HasPrefix(path, "/api/v1/")
	if v1 && kind == "" {
		panic("apiroutes: unclassified v1 registration: " + pattern)
	}
	advertise := kind == client || kind == public
	if advertise && (!v1 || !explicit || method == "" || host != "" || strings.ContainsAny(method, " \t\r\n") || !strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/")) {
		panic("apiroutes: discovery requires an explicit v1 method and exact path template: " + pattern)
	}
	// Mount first: invalid/conflicting registrations cannot leave stale metadata.
	r.mux.Handle(pattern, handler)
	if advertise {
		r.routes = append(r.routes, Route{method, path})
	}
}

func (r *Recorder) Index() Index {
	routes := append([]Route{}, r.routes...)
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Path == routes[j].Path {
			return routes[i].Method < routes[j].Method
		}
		return routes[i].Path < routes[j].Path
	})
	return Index{Routes: routes, MethodScope: "The index lists explicit methods. GET handlers determine implicit HEAD behavior. Each route enforces its existing permissions and configuration."}
}

// RegisterIndex uses the same authentication entry point as GET /api/v1/instance.
func RegisterIndex(r *Recorder, store *core.Store) {
	Client(r).HandleFunc("GET /api/v1", func(w http.ResponseWriter, req *http.Request) {
		if _, err := store.Authenticate(req); err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, r.Index())
	})
}
