package webui

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jodoherty/virtweb/internal/store"
	"github.com/jodoherty/virtweb/internal/virt"
)

// testWeb is a minimal embedded web/ tree for tests (same layout as the
// real one, stripped-down content).
var testWeb = fstest.MapFS{
	"templates/layout.html": &fstest.MapFile{Data: []byte(
		`{{define "layout"}}<html><body>{{if .Err}}<div class="banner error">{{.Err}}</div>{{end}}` +
			`{{if .OK}}<div class="banner ok">{{.OK}}</div>{{end}}` +
			`{{if eq .Page "servers"}}{{template "servers" .}}{{else if eq .Page "server"}}{{template "server" .}}` +
			`{{else if eq .Page "console"}}{{template "console" .}}{{else}}{{template "vnc" .}}{{end}}` +
			`</body></html>{{end}}`)},
	"templates/servers.html": &fstest.MapFile{Data: []byte(
		`{{define "servers"}}<ul class="server-list">{{range .Servers}}<li>{{.Name}} ` +
			`<a href="/servers/{{.ID}}">open</a></li>{{end}}</ul>` +
			`<form method="post" action="/servers"></form>{{end}}`)},
	"templates/server.html": &fstest.MapFile{Data: []byte(
		`{{define "server"}}<h1>{{.Server.Name}}</h1>{{range .VMs}}<div class="vm">{{.Name}} ` +
			`{{.State}} <a href="/servers/{{$.Server.ID}}/vms/{{q .Name}}/console">console</a></div>{{end}}` +
			`{{if not .VMs}}<p class="muted">no vms</p>{{end}}{{end}}`)},
	"templates/console.html": &fstest.MapFile{Data: []byte(
		`{{define "console"}}{{if not .Running}}<div class="banner warn">shut off</div>{{else}}` +
			`<div id="term"></div><script id="console-cfg" type="application/json">{{.CfgJSON}}</script>{{end}}{{end}}`)},
	"templates/vnc.html": &fstest.MapFile{Data: []byte(
		`{{define "vnc"}}<div id="vnc">{{.VM.Name}}</div>{{end}}`)},
	"static/style.css": &fstest.MapFile{Data: []byte("body{}")},
}

type fakeBackend struct {
	vms []virt.VM
}

func (b *fakeBackend) Ping(ctx context.Context) error { return nil }
func (b *fakeBackend) ListVMs(ctx context.Context) ([]virt.VM, error) {
	return b.vms, nil
}
func (b *fakeBackend) VMInfo(ctx context.Context, name string) (virt.VM, error) {
	for _, v := range b.vms {
		if v.Name == name {
			return v, nil
		}
	}
	return virt.VM{}, &vmNotFoundErr{name}
}

func (b *fakeBackend) Start(ctx context.Context, name string) error        { return nil }
func (b *fakeBackend) Shutdown(ctx context.Context, name string) error    { return nil }
func (b *fakeBackend) Destroy(ctx context.Context, name string) error     { return nil }
func (b *fakeBackend) Reboot(ctx context.Context, name string) error      { return nil }
func (b *fakeBackend) Console(ctx context.Context, name string, in io.Reader, out io.Writer) error {
	return nil
}
type vmNotFoundErr struct{ name string }

func (e *vmNotFoundErr) Error() string { return "domain '" + e.name + "' not found" }

type factory struct{ fb *fakeBackend }

func (f factory) NewBackend(virt.Server) virt.Backend { return f.fb }

func newTestUI(t *testing.T) (*UI, *store.Store) {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "servers.json"))
	if err != nil {
		t.Fatal(err)
	}
	fb := &fakeBackend{vms: []virt.VM{
		{Name: "debian-dev", State: "running", Running: true, IP: "10.0.2.15"},
		{Name: "win10", State: "shut off"},
	}}
	ui, err := New(st, factory{fb}, testWeb)
	if err != nil {
		t.Fatal(err)
	}
	return ui, st
}

func addServer(t *testing.T, st *store.Store) virt.Server {
	t.Helper()
	s, err := st.Add(virt.Server{Name: "s1", User: "u", Host: "h", URI: "qemu:///system"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func doGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func doForm(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func body(w *httptest.ResponseRecorder) string { return w.Body.String() }

func TestServerListPage(t *testing.T) {
	ui, st := newTestUI(t)
	addServer(t, st)
	w := doGet(t, ui.Routes(), "/")
	if w.Code != 200 || !strings.Contains(body(w), "s1") {
		t.Fatalf("got %d: %s", w.Code, body(w))
	}
}

func TestAddServerForm(t *testing.T) {
	ui, st := newTestUI(t)
	form := url.Values{}
	form.Set("name", "newhost")
	form.Set("user", "u")
	form.Set("host", "h2")
	form.Set("port", "2222")
	w := doForm(t, ui.Routes(), "/servers", form)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("got %d, want 303: %s", w.Code, body(w))
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/servers/") {
		t.Fatalf("Location = %q", loc)
	}
	id := strings.TrimPrefix(loc, "/servers/")
	srv, err := st.Get(id)
	if err != nil || srv.Name != "newhost" || srv.Port != 2222 {
		t.Fatalf("stored server %+v (err %v)", srv, err)
	}
}

func TestAddServerValidation(t *testing.T) {
	ui, _ := newTestUI(t)
	form := url.Values{}
	form.Set("name", "x") // missing user/host
	w := doForm(t, ui.Routes(), "/servers", form)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestServerPageListsVMs(t *testing.T) {
	ui, st := newTestUI(t)
	s := addServer(t, st)
	w := doGet(t, ui.Routes(), "/servers/"+s.ID)
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	b := body(w)
	if !strings.Contains(b, "debian-dev") || !strings.Contains(b, "running") ||
		!strings.Contains(b, "/vms/debian-dev/console") {
		t.Fatalf("server page missing VM table: %s", b)
	}
}

func TestServerPageShowsErrorBanner(t *testing.T) {
	ui, st := newTestUI(t)
	s := addServer(t, st)
	w := doGet(t, ui.Routes(), "/servers/"+s.ID+"?err=host%20unreachable")
	if !strings.Contains(body(w), "host unreachable") {
		t.Fatalf("missing error banner: %s", body(w))
	}
}

func TestRemoveServer(t *testing.T) {
	ui, st := newTestUI(t)
	s := addServer(t, st)
	req := httptest.NewRequest("POST", "/servers/"+s.ID+"/remove", nil)
	w := httptest.NewRecorder()
	ui.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("got %d", w.Code)
	}
	if _, err := st.Get(s.ID); err == nil {
		t.Fatal("server still present after remove")
	}
}

func TestConsolePageRunningAndShutOff(t *testing.T) {
	ui, st := newTestUI(t)
	s := addServer(t, st)
	w := doGet(t, ui.Routes(), "/servers/"+s.ID+"/vms/debian-dev/console")
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	b := body(w)
	if !strings.Contains(b, `<div id="term">`) ||
		!strings.Contains(b, `/console/stream`) || !strings.Contains(b, `/console/input`) {
		t.Fatalf("running console page wrong: %s", b)
	}
	w = doGet(t, ui.Routes(), "/servers/"+s.ID+"/vms/win10/console")
	if !strings.Contains(body(w), "shut off") {
		t.Fatalf("shut-off console page wrong: %s", body(w))
	}
}

func TestVNCPage(t *testing.T) {
	ui, st := newTestUI(t)
	s := addServer(t, st)
	w := doGet(t, ui.Routes(), "/servers/"+s.ID+"/vms/win10/vnc")
	if w.Code != 200 || !strings.Contains(body(w), "win10") {
		t.Fatalf("got %d: %s", w.Code, body(w))
	}
}

func TestStaticFiles(t *testing.T) {
	h := Static(testWeb)
	req := httptest.NewRequest("GET", "/static/style.css", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || body(w) != "body{}" {
		t.Fatalf("static serve failed: %d %q", w.Code, body(w))
	}
}

func TestUnknownServer404(t *testing.T) {
	ui, _ := newTestUI(t)
	w := doGet(t, ui.Routes(), "/servers/nope")
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d", w.Code)
	}
}
