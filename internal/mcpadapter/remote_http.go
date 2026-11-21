package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/strictjson"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	remoteBodyBytes = client.ReviewBatchMaxBytes + 16<<10
	remoteRequests  = 32
	remoteWaits     = 8
	remoteLifetime  = 930 * time.Second
)

var remoteProtocolVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"}

// RemoteConfig selects the externally visible HTTPS origin and any accepted
// Origin header values. The zero value leaves remote MCP disabled.
type RemoteConfig struct {
	Origin         string
	AllowedOrigins []string
}

func (c RemoteConfig) Validate() error {
	invalid := errors.New("Use an HTTPS origin without a path, query or fragment and at most eight explicit allowed origins.")
	if c.Origin == "" && len(c.AllowedOrigins) == 0 {
		return nil
	}
	if !validHTTPSOrigin(c.Origin) || len(c.AllowedOrigins) > 8 {
		return invalid
	}
	for _, origin := range c.AllowedOrigins {
		if !validHTTPSOrigin(origin) {
			return invalid
		}
	}
	return nil
}

func validHTTPSOrigin(origin string) bool {
	if len(origin) > 512 || !strings.HasPrefix(origin, "https://") {
		return false
	}
	u, err := url.Parse(origin)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && origin == "https://"+u.Host
}

type connectorRequestKey struct{}

type connectorRequest struct {
	identity profile.Context
	binding  core.ConnectorBinding
	access   core.ConnectorAccess
	token    string
	lifetime context.Context
}

type remoteMCP struct {
	store       *core.Store
	destination string
	pool        *client.TransportPool
	waits       chan struct{}
}

// NewRemoteHandler exposes the same typed tool dispatch as stdio through an
// authenticated stateless SDK server. Use the daemon's bound loopback address.
func NewRemoteHandler(lifetime context.Context, store *core.Store, c RemoteConfig, address string) (http.Handler, func(), error) {
	if err := c.Validate(); err != nil {
		return nil, nil, err
	}
	host, port, err := net.SplitHostPort(address)
	if c.Origin == "" || err != nil || !net.ParseIP(host).IsLoopback() || port == "" || port == "0" {
		return nil, nil, errors.New("Remote MCP requires an explicit origin and a bound loopback listener.")
	}
	publicHost := strings.TrimPrefix(c.Origin, "https://")
	allowedOrigins := append([]string{c.Origin}, c.AllowedOrigins...)
	remote := &remoteMCP{
		store: store, destination: "http://" + address,
		pool: client.NewTransportPool(), waits: make(chan struct{}, remoteWaits),
	}
	server := toolServer(profile.Binding{}, remote)
	multiServer := connectorToolServer(profile.Binding{}, remote, true)
	streamable := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		if request, ok := r.Context().Value(connectorRequestKey{}).(*connectorRequest); ok && request.access.MultiProject {
			return multiServer
		}
		return server
	}, &mcp.StreamableHTTPOptions{
		Stateless: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		// The wrapper checks the explicitly configured public Host and Origin.
		DisableLocalhostProtection: true, MaxRequestBodyBytes: remoteBodyBytes,
		PropagateRequestCancellation: true,
	})
	requests := make(chan struct{}, remoteRequests)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !strings.EqualFold(r.Host, publicHost) {
			core.WriteError(w, core.Fail("forbidden", "The requested MCP host is unavailable.", http.StatusForbidden))
			return
		}
		origins := r.Header.Values("Origin")
		if len(origins) > 1 || len(origins) == 1 && !slices.Contains(allowedOrigins, origins[0]) {
			core.WriteError(w, core.Fail("forbidden", "The MCP origin is not allowed.", http.StatusForbidden))
			return
		}
		for name := range r.Header {
			if strings.HasPrefix(strings.ToLower(name), "x-fc-") {
				core.WriteError(w, core.Fail("validation_failed", "The MCP connection does not accept FireController identity headers.", http.StatusBadRequest))
				return
			}
		}
		select {
		case requests <- struct{}{}:
			defer func() { <-requests }()
		default:
			core.WriteError(w, core.Fail("overloaded", "Remote MCP request capacity is occupied.", http.StatusTooManyRequests))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), remoteLifetime)
		defer cancel()
		stop := context.AfterFunc(lifetime, cancel)
		defer stop()
		authCtx, cancelAuth := context.WithTimeout(ctx, 2*time.Second)
		access, token, err := store.AuthenticateConnector(r.WithContext(authCtx))
		cancelAuth()
		if err == nil && (len(access.Bindings) == 0 || !access.MultiProject && len(access.Bindings) != 1) {
			err = core.Fail("unauthorized", "A valid connector credential is required.", http.StatusUnauthorized)
		}
		if err != nil {
			var failure *core.Error
			if errors.As(err, &failure) && failure.Status == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", `Bearer realm="FireController MCP"`)
			}
			core.WriteError(w, err)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			core.WriteError(w, core.Fail("method_not_allowed", "Use POST for stateless MCP.", http.StatusMethodNotAllowed))
			return
		}
		if r.URL.RawQuery != "" || r.URL.ForceQuery || len(r.Header.Values("Mcp-Session-Id")) != 0 {
			core.WriteError(w, core.Fail("validation_failed", "Use the MCP resource URL without a query or session selector.", http.StatusBadRequest))
			return
		}
		versions := r.Header.Values("Mcp-Protocol-Version")
		if len(versions) > 1 || len(versions) == 1 && !slices.Contains(remoteProtocolVersions, versions[0]) {
			core.WriteError(w, core.Fail("validation_failed", "Select a supported MCP protocol version.", http.StatusBadRequest))
			return
		}
		if !readRemoteEnvelope(w, r, r.Header.Get("Mcp-Protocol-Version")) {
			return
		}
		controller := http.NewResponseController(w)
		deadline, _ := ctx.Deadline()
		if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return
		}
		request := &connectorRequest{
			identity: profile.Context{Profile: "connector", ServerURL: c.Origin, InstanceID: store.InstanceID()},
			access:   access, token: token, lifetime: ctx,
		}
		if !access.MultiProject {
			request.binding = access.Bindings[0]
			request.identity.ProjectID = request.binding.ProjectID
			request.identity.WorkerID = request.binding.WorkerID
		}
		r = r.Clone(context.WithValue(ctx, connectorRequestKey{}, request))
		r.Header.Del("Authorization")
		streamable.ServeHTTP(privateRemoteResponse{w}, r)
	})
	return handler, remote.pool.Close, nil
}

// The SDK sets cache directives for event streams. Preserve no-store for
// both its protocol responses and streams carrying private tool results.
type privateRemoteResponse struct{ http.ResponseWriter }

func (w privateRemoteResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w privateRemoteResponse) WriteHeader(status int) {
	w.Header().Set("Cache-Control", "no-store, no-transform")
	w.ResponseWriter.WriteHeader(status)
}
func (w privateRemoteResponse) Write(data []byte) (int, error) {
	w.Header().Set("Cache-Control", "no-store, no-transform")
	return w.ResponseWriter.Write(data)
}
func (w privateRemoteResponse) Flush() {
	w.Header().Set("Cache-Control", "no-store, no-transform")
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func readRemoteEnvelope(w http.ResponseWriter, r *http.Request, version string) bool {
	limited := http.MaxBytesReader(w, r.Body, remoteBodyBytes)
	body, err := io.ReadAll(limited)
	_ = limited.Close()
	var sizeError *http.MaxBytesError
	if r.ContentLength > remoteBodyBytes || errors.As(err, &sizeError) {
		core.WriteError(w, core.Fail("request_too_large", "The MCP request body exceeds its limit.", http.StatusRequestEntityTooLarge))
		return false
	}
	trimmed := bytes.TrimSpace(body)
	// Single request admission prevents a legacy batch from multiplying the
	// work admitted by one HTTP slot.
	if err != nil || len(trimmed) == 0 || trimmed[0] != '{' || strictjson.Validate(body) != nil {
		core.WriteError(w, core.Fail("validation_failed", "Send one strict JSON-RPC object per POST.", http.StatusBadRequest))
		return false
	}
	message, err := jsonrpc.DecodeMessage(body)
	request, ok := message.(*jsonrpc.Request)
	if err != nil || !ok {
		core.WriteError(w, core.Fail("validation_failed", "Send one JSON-RPC request or notification per POST.", http.StatusBadRequest))
		return false
	}
	switch request.Method {
	case "server/discover", "initialize", "notifications/initialized", "notifications/cancelled", "ping", "tools/list", "tools/call":
	default:
		core.WriteError(w, core.Fail("method_not_allowed", "The requested MCP method is unavailable.", http.StatusBadRequest))
		return false
	}
	// The SDK's header mismatch text includes the supplied values. Check its
	// modern method and tool selectors here so the refusal stays generic.
	if version == "2026-07-28" && !matchingMCPHeaders(r.Header, request) {
		core.WriteError(w, core.Fail("validation_failed", "The MCP selectors must match the request.", http.StatusBadRequest))
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return true
}

func matchingMCPHeaders(headers http.Header, request *jsonrpc.Request) bool {
	method := headers.Values("Mcp-Method")
	if len(method) != 1 || method[0] != request.Method {
		return false
	}
	if request.Method != "tools/call" {
		return true
	}
	var params struct {
		Name string `json:"name"`
	}
	name := headers.Values("Mcp-Name")
	return len(name) == 1 && name[0] != "" && json.Unmarshal(request.Params, &params) == nil && name[0] == params.Name
}

func connectorAccessChanged(write bool) *Diagnostic {
	d := diagnostic("connector_access_changed", "The connector could not verify current project access before delivering the result.", "Verify the original connector binding and its current access before retrying.")
	if write {
		d.OutcomeUnknown = true
		d.Action += " Recover the original operation with its exact arguments and idempotency key."
	}
	return d
}
