// Package webui serves the server-rendered pages of virtweb: plain
// HTML templates + forms (PRG redirects) for everything except the
// serial console (xterm.js over the /api streams transport) and the
// VNC viewer (noVNC). The JSON API in internal/api remains for
// automation and the e2e suite.
//
// The templates and static files live in the web/ directory, embedded
// by main and handed in here (tests can supply fstest.MapFS fixtures).
package webui

import (
	"context"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/jodoherty/virtweb/internal/store"
	"github.com/jodoherty/virtweb/internal/virt"
)

var funcs = template.FuncMap{
	// q escapes a name for use in a URL path segment.
	"q": url.PathEscape,
}

// BackendFactory creates (or returns a cached) backend for a server —
// the same seam internal/api uses.
type BackendFactory interface {
	NewBackend(srv virt.Server) virt.Backend
}

// UI renders the web pages.
type UI struct {
	st       *store.Store
	factory  BackendFactory
	tpl      *template.Template
	mu       sync.Mutex
	backends map[string]virt.Backend
}

// New builds the UI over st and factory. web is the embedded web/
// directory (must contain templates/ and static/). Each content
// template defines its own page name ({{define "servers"}}, ...); the
// layout selects between them.
func New(st *store.Store, factory BackendFactory, web fs.FS) (*UI, error) {
	tpl, err := template.New("layout.html").Funcs(funcs).ParseFS(web, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &UI{st: st, factory: factory, tpl: tpl,
		backends: map[string]virt.Backend{}}, nil
}

// Routes returns a handler for the page routes.
func (u *UI) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", u.handleServerList)
	mux.HandleFunc("POST /servers", u.handleAddServer)
	mux.HandleFunc("GET /servers/{id}", u.handleServer)
	mux.HandleFunc("GET /servers/{id}/vms/{name}", u.handleVMPage)
	mux.HandleFunc("POST /servers/{id}/update", u.handleUpdateServer)
	mux.HandleFunc("POST /servers/{id}/remove", u.handleRemoveServer)
	mux.HandleFunc("POST /servers/{id}/vms/{name}/{action}", u.handleVMAction)
	mux.HandleFunc("GET /servers/{id}/vms/{name}/console", u.handleConsolePage)
	mux.HandleFunc("GET /servers/{id}/vms/{name}/vnc", u.handleVNCPage)
	mux.HandleFunc("GET /servers/{id}/vms/{name}/spice", u.handleSpicePage)
	return mux
}

// Static serves the static web files (css, page scripts) from the
// same embedded web/ directory.
func Static(web fs.FS) http.Handler {
	sub, err := fs.Sub(web, "static")
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.StripPrefix("/static/", http.FileServerFS(sub))
}

// page is the render context for every template.
type page struct {
	Title   string
	Page    string
	Err     string
	OK      string
	Servers []virt.Server
	Server  *virt.Server
	VMs     []virt.VM
	VM      *virt.VM
	Running bool
	// XML is the domain XML shown on the VM detail page (virsh dumpxml).
	XML string
	// CfgJSON is the console page's {streamUrl, inputUrl} config, JSON
	// produced by json.Marshal over PathEscaped URLs. Marked template.JS
	// so it is emitted raw inside the <script> (no re-escaping into a JS
	// string literal); no unescaped user input reaches it.
	CfgJSON template.JS
}

func (u *UI) render(w http.ResponseWriter, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	u.tpl.ExecuteTemplate(w, "layout", p)
}

func (u *UI) backend(id string) (virt.Server, virt.Backend, error) {
	srv, err := u.st.Get(id)
	if err != nil {
		return virt.Server{}, nil, err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if b, ok := u.backends[id]; ok {
		return srv, b, nil
	}
	b := u.factory.NewBackend(srv)
	u.backends[id] = b
	return srv, b, nil
}

func (u *UI) handleServerList(w http.ResponseWriter, r *http.Request) {
	u.render(w, page{
		Page:    "servers",
		Title:   "Servers",
		Err:     queryError(r),
		Servers: u.st.List(),
	})
}

func (u *UI) handleAddServer(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	f := r.Form
	srv := virt.Server{
		Name:         strings.TrimSpace(f.Get("name")),
		User:         strings.TrimSpace(f.Get("user")),
		Host:         strings.TrimSpace(f.Get("host")),
		URI:          f.Get("uri"),
		IdentityFile: strings.TrimSpace(f.Get("identityFile")),
		Password:     strings.TrimSpace(f.Get("password")),
	}
	if p := strings.TrimSpace(f.Get("port")); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			http.Error(w, "invalid port", http.StatusBadRequest)
			return
		}
		srv.Port = n
	}
	if srv.Name == "" || srv.User == "" || srv.Host == "" {
		http.Error(w, "name, user and host are required", http.StatusBadRequest)
		return
	}
	if srv.URI == "" {
		srv.URI = "qemu:///system"
	}
	created, err := u.st.Add(srv)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/servers/"+created.ID, http.StatusSeeOther)
}

func (u *UI) handleUpdateServer(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	f := r.Form
	srv := virt.Server{
		Name:         strings.TrimSpace(f.Get("name")),
		User:         strings.TrimSpace(f.Get("user")),
		Host:         strings.TrimSpace(f.Get("host")),
		URI:          f.Get("uri"),
		IdentityFile: strings.TrimSpace(f.Get("identityFile")),
		Password:     strings.TrimSpace(f.Get("password")),
	}
	if p := strings.TrimSpace(f.Get("port")); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			http.Error(w, "invalid port", http.StatusBadRequest)
			return
		}
		srv.Port = n
	}
	if srv.Name == "" || srv.User == "" || srv.Host == "" {
		http.Error(w, "name, user and host are required", http.StatusBadRequest)
		return
	}
	if srv.URI == "" {
		srv.URI = "qemu:///system"
	}
	id := r.PathValue("id")
	updated, err := u.st.Update(id, srv)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The host definition changed: drop the cached backend so the next
	// call rebuilds it against the new host/port.
	u.mu.Lock()
	delete(u.backends, id)
	u.mu.Unlock()
	http.Redirect(w, r, "/servers/"+updated.ID+"?ok="+url.QueryEscape("server updated"),
		http.StatusSeeOther)
}

func (u *UI) handleRemoveServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	u.mu.Lock()
	delete(u.backends, id)
	u.mu.Unlock()
	if err := u.st.Remove(id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (u *UI) handleServer(w http.ResponseWriter, r *http.Request) {
	srv, b, err := u.backend(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p := page{Page: "server", Title: srv.Name, OK: r.URL.Query().Get("ok"), Err: queryError(r), Server: &srv}
	vms, err := b.ListVMs(r.Context())
	if err != nil {
		p.Err = "list VMs: " + err.Error()
	} else {
		p.VMs = vms
	}
	u.render(w, p)
}

// handleVMPage renders the per-VM detail page: live state from VMInfo
// and the current domain XML from DumpXML.
func (u *UI) handleVMPage(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("name")
	srv, b, err := u.backend(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	vm, err := b.VMInfo(r.Context(), name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p := page{Page: "vm", Title: name, OK: r.URL.Query().Get("ok"), Err: queryError(r),
		Server: &srv, VM: &vm}
	if xml, err := b.DumpXML(r.Context(), name); err != nil {
		if p.Err == "" {
			p.Err = "xml: " + err.Error()
		}
	} else {
		p.XML = xml
	}
	// The console-in-use badge polls this one-shot status endpoint on
	// page load (the Refresh link re-fetches it).
	cj, _ := json.Marshal(map[string]string{
		"consoleStatusUrl": "/api/servers/" + id + "/vms/" + url.PathEscape(name) + "/console/status",
	})
	p.CfgJSON = template.JS(cj)
	u.render(w, p)
}

func (u *UI) handleVMAction(w http.ResponseWriter, r *http.Request) {
	id, name, action := r.PathValue("id"), r.PathValue("name"), r.PathValue("action")
	_, b, err := u.backend(id)
	if err != nil {
		http.NotFound(w, r)
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
		http.NotFound(w, r)
		return
	}
	if err := fn(r.Context(), name); err != nil {
		http.Redirect(w, r, "/servers/"+id+"?err="+url.QueryEscape(action+" "+name+": "+err.Error()),
			http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/servers/"+id+"?ok="+url.QueryEscape(action+" "+name),
		http.StatusSeeOther)
}

func (u *UI) handleConsolePage(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("name")
	srv, b, err := u.backend(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	vm, err := b.VMInfo(r.Context(), name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	cfg, _ := json.Marshal(map[string]string{
		"streamUrl": "/api/servers/" + id + "/vms/" + url.PathEscape(name) + "/console/stream",
		"inputUrl":  "/api/servers/" + id + "/vms/" + url.PathEscape(name) + "/console/input",
	})
	u.render(w, page{
		Page:    "console",
		Title:   "Console " + vm.Name,
		Server:  &srv,
		VM:      &vm,
		Running: vm.Running,
		CfgJSON: template.JS(cfg),
	})
}

func (u *UI) handleVNCPage(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("name")
	srv, b, err := u.backend(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	vm, err := b.VMInfo(r.Context(), name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	cfg := map[string]any{"streamUrl": "", "inputUrl": "", "password": ""}
	if vm.VNCPort > 0 {
		base := "/api/servers/" + id + "/vms/" + url.PathEscape(name) + "/tunnel"
		// The destination (srv.Host:port) is resolved server-side from
		// this VM's live VNC port; the client only names the display.
		q := "?display=vnc"
		cfg["streamUrl"] = base + "/stream" + q
		cfg["inputUrl"] = base + "/input" + q
	} else {
		cfg["reason"] = "VNC is not enabled on this VM (no VNC port reported)."
	}
	j, _ := json.Marshal(cfg)
	u.render(w, page{
		Page:    "vnc",
		Title:   "VNC " + vm.Name,
		Server:  &srv,
		VM:      &vm,
		Running: vm.Running,
		CfgJSON: template.JS(j),
	})
}

func (u *UI) handleSpicePage(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("name")
	srv, b, err := u.backend(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	vm, err := b.VMInfo(r.Context(), name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// The SPICE port is auto-assigned at boot and the display is often
	// loopback-bound, so the address comes from `virsh domdisplay`
	// (vm.SpiceAddr). It is shown on the page for information, but the
	// tunnel destination is resolved server-side (?display=spice) — the
	// client never supplies a host:port.
	j, _ := json.Marshal(map[string]string{
		"tunnelBase": "/api/servers/" + id + "/vms/" + url.PathEscape(name) + "/tunnel",
		"defaultAddr": vm.SpiceAddr,
	})
	u.render(w, page{
		Page:    "spice",
		Title:   "SPICE " + vm.Name,
		Server:  &srv,
		VM:      &vm,
		Running: vm.Running,
		CfgJSON: template.JS(j),
	})
}

func queryError(r *http.Request) string {
	return r.URL.Query().Get("err")
}
