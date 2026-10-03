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
	"sync"

	"github.com/jodoherty/virtweb/internal/store"
	"github.com/jodoherty/virtweb/internal/virt"
)

// BackendFactory creates (or returns a cached) backend for a server.
type BackendFactory interface {
	NewBackend(srv virt.Server) virt.Backend
}

// API is the REST API server.
type API struct {
	st        *store.Store
	factory   BackendFactory
	mu        sync.Mutex
	backends  map[string]virt.Backend
	conMu     sync.Mutex
	consoles  map[string]*pipeSession
	tunMu     sync.Mutex
	tunnels   map[string]*pipeSession
}

// New builds an API over the given store and factory.
func New(st *store.Store, factory BackendFactory) *API {
	return &API{st: st, factory: factory,
		backends: map[string]virt.Backend{},
		consoles: map[string]*pipeSession{},
		tunnels:  map[string]*pipeSession{}}
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
	mux.HandleFunc("DELETE /api/servers/{id}", a.handleRemoveServer)
	mux.HandleFunc("GET /api/servers/{id}/vms", a.handleListVMs)
	mux.HandleFunc("POST /api/servers/{id}/vms/{name}/{action}", a.handleVMAction)
	mux.HandleFunc("GET /api/servers/{id}/ping", a.handlePing)
	mux.HandleFunc("GET /api/servers/{id}/vms/{name}/console/stream", a.handleConsoleStream)
	mux.HandleFunc("POST /api/servers/{id}/vms/{name}/console/input", a.handleConsoleInput)
	mux.HandleFunc("GET /api/servers/{id}/vms/{name}/tunnel/stream", a.handleTunnelStream)
	mux.HandleFunc("POST /api/servers/{id}/vms/{name}/tunnel/input", a.handleTunnelInput)
	return mux
}

/* ---------- handlers ---------- */

func (a *API) handleListServers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.st.List())
}

func (a *API) handleAddServer(w http.ResponseWriter, r *http.Request) {
	var srv virt.Server
	if err := json.NewDecoder(r.Body).Decode(&srv); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: %v", err)
		return
	}
	created, err := a.st.Add(srv)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (a *API) handleGetServer(w http.ResponseWriter, r *http.Request) {
	srv, err := a.st.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, srv)
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
