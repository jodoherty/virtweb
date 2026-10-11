// Package api exposes the REST API used by the web frontend. It depends
// only on the store and the virt.BackendFactory, so tests can supply a
// fake factory.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jodoherty/virtweb/internal/store"
	"github.com/jodoherty/virtweb/internal/virt"
	"github.com/jodoherty/virtweb/internal/ws"
)

// BackendFactory creates (or returns a cached) backend for a server.
type BackendFactory interface {
	NewBackend(srv virt.Server) virt.Backend
}

// API is the REST API server.
type API struct {
	st      *store.Store
	factory BackendFactory
	mu      sync.Mutex
	backends map[string]virt.Backend
	conMu    sync.Mutex
	consoles map[string]*pipeSession
	tunMu    sync.Mutex
	tunnels  map[string]*pipeSession
	// maxConsoles/maxTunnels bound live byte-stream sessions (a DoS
	// cap: each session holds a remote ssh process). 429 when reached.
	maxConsoles int
	maxTunnels  int
	// keepalive is the h1 session keepalive window (-keepalive flag,
	// 0 disables); see stream.go.
	keepalive time.Duration
	// upH1 serves the plain HTTP/1.1 Upgrade leg (keepalive window on:
	// a hijacked net.Conn always enforces deadlines). upH2 serves the
	// HTTP/2 extended-CONNECT leg, where the streams cannot enforce
	// deadlines, so both windows are zero — the library refuses a
	// nonzero window there (see stream.go).
	// The origin policy is the library default: strict same-origin for
	// requests that carry an Origin header (a cross-origin browser is
	// the cross-site WebSocket hijacking case — mTLS authenticates the
	// browser, not the page), no Origin allowed (tooling).
	upH1   *ws.Upgrader
	upH2   *ws.Upgrader
	h1mu   sync.Mutex
	h1warn map[string]*h1Warn
}

// Option configures an API.
type Option func(*API)

// DefaultKeepalive is the stream keepalive interval used when no
// WithKeepalive option is given (see stream.go).
const DefaultKeepalive = 15 * time.Second

// WithKeepalive sets the HTTP/1.1 session keepalive window (see
// stream.go); 0 disables idle keepalives.
func WithKeepalive(d time.Duration) Option {
	return func(a *API) { a.keepalive = d }
}

// New builds an API over the given store and factory.
func New(st *store.Store, factory BackendFactory, opts ...Option) *API {
	a := &API{st: st, factory: factory,
		backends:    map[string]virt.Backend{},
		consoles:    map[string]*pipeSession{},
		tunnels:     map[string]*pipeSession{},
		maxConsoles: 64,
		maxTunnels:  64,
		keepalive:   DefaultKeepalive,
		h1warn:      map[string]*h1Warn{}}
	for _, opt := range opts {
		opt(a)
	}
	a.upH1 = ws.NewUpgrader(
		ws.WithIdleTimeout(a.keepalive),
		ws.WithMaxMessageSize(maxWSMessage))
	a.upH2 = ws.NewUpgrader(
		ws.WithIdleTimeout(0),
		ws.WithWriteTimeout(0),
		ws.WithMaxMessageSize(maxWSMessage))
	return a
}

// backend returns the cached backend for a server id.
func (a *API) backend(id string) (virt.Server, virt.Backend, error) {
	srv, err := a.st.Get(id)
	if err != nil {
		return virt.Server{}, nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if b, ok := a.backends[id]; ok {
		return srv, b, nil
	}
	b := a.factory.NewBackend(srv)
	a.backends[id] = b
	return srv, b, nil
}

// Routes returns the /api handler.
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/servers", a.handleListServers)
	mux.HandleFunc("POST /api/servers", a.handleAddServer)
	mux.HandleFunc("GET /api/servers/{id}", a.handleGetServer)
	mux.HandleFunc("PUT /api/servers/{id}", a.handleUpdateServer)
	mux.HandleFunc("DELETE /api/servers/{id}", a.handleRemoveServer)
	mux.HandleFunc("GET /api/servers/{id}/vms", a.handleListVMs)
	mux.HandleFunc("POST /api/servers/{id}/vms", a.handleDefineVM)
	mux.HandleFunc("DELETE /api/servers/{id}/vms/{name}", a.handleUndefineVM)
	mux.HandleFunc("GET /api/servers/{id}/vms/{name}/xml", a.handleVMXML)
	mux.HandleFunc("POST /api/servers/{id}/vms/{name}/{action}", a.handleVMAction)
	mux.HandleFunc("GET /api/servers/{id}/ping", a.handlePing)
	// Each stream endpoint serves both legs: the GET pattern takes the
	// HTTP/1.1 Upgrade (and HEAD, which the mux folds into GET) —
	// the latter is the browser's preflight; the CONNECT pattern takes
	// the HTTP/2 extended-CONNECT leg (RFC 8441).
	mux.HandleFunc("GET /api/servers/{id}/vms/{name}/console/stream", a.handleConsoleStream)
	mux.HandleFunc("CONNECT /api/servers/{id}/vms/{name}/console/stream", a.handleConsoleStream)
	mux.HandleFunc("GET /api/servers/{id}/vms/{name}/console/status", a.handleConsoleStatus)
	mux.HandleFunc("GET /api/servers/{id}/vms/{name}/tunnel/stream", a.handleTunnelStream)
	mux.HandleFunc("CONNECT /api/servers/{id}/vms/{name}/tunnel/stream", a.handleTunnelStream)
	mux.HandleFunc("GET /api/servers/{id}/vms/{name}/tunnel/status", a.handleTunnelStatus)
	return mux
}

/* ---------- handlers ---------- */

// serverView is the JSON shape of a server for responses: the private
// key material is never returned (the UI needs to know only that one
// is stored); the password is returned as-is so the edit form can
// pre-fill (the deployment model assumes a trusted, mTLS path).
type serverView struct {
	virt.Server
	KeySet bool `json:"keySet"`
}

func toServerView(srv virt.Server) serverView {
	keySet := srv.Key != ""
	srv.Key = ""
	return serverView{srv, keySet}
}

func (a *API) handleListServers(w http.ResponseWriter, r *http.Request) {
	views := make([]serverView, 0, len(a.st.List()))
	for _, srv := range a.st.List() {
		views = append(views, toServerView(srv))
	}
	writeJSON(w, http.StatusOK, views)
}

func (a *API) handleAddServer(w http.ResponseWriter, r *http.Request) {
	// A server definition is a few short fields; cap the body so a
	// hostile client cannot pin memory with a giant JSON payload.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var srv virt.Server
	if err := json.NewDecoder(r.Body).Decode(&srv); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: %v", err)
		return
	}
	if err := store.KeySyntaxError(srv.Key); err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	created, err := a.st.Add(srv)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(w, http.StatusCreated, toServerView(created))
}

func (a *API) handleGetServer(w http.ResponseWriter, r *http.Request) {
	srv, err := a.st.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, toServerView(srv))
}

func (a *API) handleUpdateServer(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var srv virt.Server
	if err := json.NewDecoder(r.Body).Decode(&srv); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: %v", err)
		return
	}
	if err := store.KeySyntaxError(srv.Key); err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	updated, err := a.st.Update(r.PathValue("id"), srv)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeErr(w, http.StatusNotFound, "%v", err)
			return
		}
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	// The host definition changed: drop the cached backend so the next
	// call rebuilds it against the new host/port.
	a.mu.Lock()
	delete(a.backends, updated.ID)
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, toServerView(updated))
}

func (a *API) handleRemoveServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.mu.Lock()
	delete(a.backends, id)
	a.mu.Unlock()
	if err := a.st.Remove(id); err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDefineVM creates a domain from a libvirt domain XML document.
// The XML travels in the JSON body; name is derived from it. An
// optional "start": true starts the domain right after defining it.
func (a *API) handleDefineVM(w http.ResponseWriter, r *http.Request) {
	_, b, err := a.backend(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		XML   string `json:"xml"`
		Start bool   `json:"start"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: %v", err)
		return
	}
	name := virt.DomainName(req.XML)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "invalid domain XML: missing <name> element")
		return
	}
	if err := b.Define(r.Context(), req.XML); err != nil {
		writeErr(w, http.StatusBadGateway, "define %s: %v", name, err)
		return
	}
	if req.Start {
		if err := b.Start(r.Context(), name); err != nil {
			// The domain is defined (visible in the list); the start
			// failure is reported alongside.
			writeJSON(w, http.StatusCreated, map[string]any{
				"name": name, "started": false, "error": err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"name": name, "started": true})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"name": name})
}

func (a *API) handleListVMs(w http.ResponseWriter, r *http.Request) {
	_, b, err := a.backend(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}
	vms, err := b.ListVMs(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list VMs: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, vms)
}

func (a *API) handlePing(w http.ResponseWriter, r *http.Request) {
	_, b, err := a.backend(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}
	if err := b.Ping(r.Context()); err != nil {
		writeErr(w, http.StatusBadGateway, "ping: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleUndefineVM removes a domain definition (virsh undefine); the
// domain must be shut off (destroy it first).
func (a *API) handleUndefineVM(w http.ResponseWriter, r *http.Request) {
	_, b, err := a.backend(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}
	if err := b.Undefine(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, http.StatusBadGateway, "undefine: %v", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleVMXML returns the domain's current libvirt XML (virsh dumpxml).
func (a *API) handleVMXML(w http.ResponseWriter, r *http.Request) {
	_, b, err := a.backend(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}
	xml, err := b.DumpXML(r.Context(), r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "dumpxml: %v", err)
		return
	}
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Write([]byte(xml))
}

func (a *API) handleVMAction(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	action := r.PathValue("action")
	_, b, err := a.backend(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}
	var fn func(context.Context, string) error
	switch action {
	case "start":
		fn = b.Start
	case "shutdown":
		fn = b.Shutdown
	case "destroy":
		fn = b.Destroy
	case "reboot":
		fn = b.Reboot
	default:
		writeErr(w, http.StatusNotFound, "unknown action %q", action)
		return
	}
	if err := fn(r.Context(), name); err != nil {
		writeErr(w, http.StatusBadGateway, "%s %s: %v", action, name, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

/* ---------- helpers ---------- */

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, format string, args ...any) {
	// The body must be small enough to fit in one h2 frame; keep it terse.
	log.Printf("api %s", fmt.Sprintf(format, args...))
	msg := fmt.Sprintf(format, args...)
	if len(msg) > 1024 {
		msg = msg[:1024]
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
