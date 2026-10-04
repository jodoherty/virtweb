package webui

import (
	"context"
	"errors"
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
const testKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nfake-key\n-----END OPENSSH PRIVATE KEY-----"

var testWeb = fstest.MapFS{
	"templates/layout.html": &fstest.MapFile{Data: []byte(
		`{{define "layout"}}<html><body>{{if .Err}}<div class="banner error">{{.Err}}</div>{{end}}` +
			`{{if .OK}}<div class="banner ok">{{.OK}}</div>{{end}}` +
			`{{if eq .Page "servers"}}{{template "servers" .}}{{else if eq .Page "server"}}{{template "server" .}}` +
			`{{else if eq .Page "vm"}}{{template "vm" .}}{{else if eq .Page "console"}}{{template "console" .}}` +
			`{{else}}{{template "vnc" .}}{{end}}` +
			`</body></html>{{end}}`)},
	"templates/vm.html": &fstest.MapFile{Data: []byte(
		`{{define "vm"}}<h2>{{.VM.Name}}</h2>` +
			`<form method="post" action="/servers/{{.Server.ID}}/vms/{{.VM.Name}}/start"></form>` +
			`<form method="post" action="/servers/{{.Server.ID}}/vms/{{.VM.Name}}/shutdown" data-confirm="Shut down?"></form>` +
			`<form method="post" action="/servers/{{.Server.ID}}/vms/{{.VM.Name}}/destroy" data-confirm="Destroy now?"></form>` +
			`<form method="post" action="/servers/{{.Server.ID}}/vms/{{.VM.Name}}/reboot" data-confirm="Reboot?"></form>` +
			`{{if .VM.Running}}<a href="/servers/{{.Server.ID}}/vms/{{.VM.Name}}/console">console</a>` +
			`{{if .VM.VNCPort}}<a href="/servers/{{.Server.ID}}/vms/{{.VM.Name}}/vnc">vnc</a>{{end}}` +
			`{{if .VM.SpiceAddr}}<a href="/servers/{{.Server.ID}}/vms/{{.VM.Name}}/spice">spice</a>{{end}}{{end}}` +
			`<pre id="vm-xml">{{.XML}}</pre>` +
			`<script id="vm-cfg" type="application/json">{{.CfgJSON}}</script>{{end}}`)},
	"templates/servers.html": &fstest.MapFile{Data: []byte(
		`{{define "servers"}}<ul class="server-list">{{range .Servers}}<li>{{.Name}} ` +
			`<a href="/servers/{{.ID}}">open</a></li>{{end}}</ul>` +
			`<form method="post" action="/servers"><textarea name="key"></textarea>` +
			`<input name="password" type="password"></form>{{end}}`)},
	"templates/server.html": &fstest.MapFile{Data: []byte(
		`{{define "server"}}<h1>{{.Server.Name}}</h1>{{range .VMs}}<div class="vm">{{.Name}} ` +
			`{{.State}}` +
			`<a href="/servers/{{$.Server.ID}}/vms/{{q .Name}}">{{.Name}}</a>` +
			`{{if .Running}}<a href="/servers/{{$.Server.ID}}/vms/{{q .Name}}/console">console</a>{{end}}</div>{{end}}` +
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
func (b *fakeBackend) DumpXML(ctx context.Context, name string) (string, error) {
	for _, v := range b.vms {
		if v.Name == name {
			return "<domain type='kvm'><name>" + name + "</name></domain>", nil
		}
	}
	return "", &vmNotFoundErr{name}
}

func (b *fakeBackend) Start(ctx context.Context, name string) error        { return nil }
func (b *fakeBackend) Shutdown(ctx context.Context, name string) error    { return nil }
func (b *fakeBackend) Destroy(ctx context.Context, name string) error     { return nil }
func (b *fakeBackend) Reboot(ctx context.Context, name string) error      { return nil }
func (b *fakeBackend) Define(ctx context.Context, xml string) error {
	name := virt.DomainName(xml)
	if name == "" {
		return errors.New("no domain name in XML")
	}
	b.vms = append(b.vms, virt.VM{Name: name, State: "shut off"})
	return nil
}
func (b *fakeBackend) Undefine(ctx context.Context, name string) error {
	for i, v := range b.vms {
		if v.Name == name {
			b.vms = append(b.vms[:i], b.vms[i+1:]...)
			return nil
		}
	}
	return &vmNotFoundErr{name}
}
func (b *fakeBackend) Console(ctx context.Context, name string, in io.Reader, out io.Writer) error {
	return nil
}
func (b *fakeBackend) Tunnel(ctx context.Context, name, target string, in io.Reader, out io.Writer) error {
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
		{Name: "debian-dev", State: "running", Running: true, IP: "10.0.2.15", SpiceAddr: "stub:5991"},
		{Name: "win10", State: "shut off", VNCPort: 5901},
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
	form.Set("key", testKey)
	form.Set("password", "s3cret")
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
	if err != nil || srv.Name != "newhost" || srv.Port != 2222 ||
		srv.Key != testKey || srv.Password != "s3cret" {
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

func TestUpdateServerForm(t *testing.T) {
	ui, st := newTestUI(t)
	srv := addServer(t, st)

	form := url.Values{}
	form.Set("name", "renamed")
	form.Set("user", "u2")
	form.Set("host", "h2")
	form.Set("port", "2222")
	form.Set("uri", "qemu:///system")
	form.Set("key", testKey)
	w := doForm(t, ui.Routes(), "/servers/"+srv.ID+"/update", form)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("got %d, want 303: %s", w.Code, body(w))
	}
	got, err := st.Get(srv.ID) // id must be preserved
	if err != nil || got.Name != "renamed" || got.Host != "h2" || got.Port != 2222 ||
		got.Key != testKey {
		t.Fatalf("stored server %+v (err %v)", got, err)
	}

	// clearing the credential fields drops them from the definition.
	form2c := url.Values{}
	form2c.Set("name", "renamed")
	form2c.Set("user", "u2")
	form2c.Set("host", "h2")
	w = doForm(t, ui.Routes(), "/servers/"+srv.ID+"/update", form2c)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("got %d, want 303", w.Code)
	}
	got, err = st.Get(srv.ID)
	if err != nil || got.Key != "" || got.Password != "" {
		t.Fatalf("credential not cleared: %+v (err %v)", got, err)
	}

	// removeKey drops the stored key without a replacement.
	form3 := url.Values{}
	form3.Set("name", "renamed")
	form3.Set("user", "u2")
	form3.Set("host", "h2")
	form3.Set("key", testKey)
	w = doForm(t, ui.Routes(), "/servers/"+srv.ID+"/update", form3)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("re-store key got %d, want 303", w.Code)
	}
	got, err = st.Get(srv.ID)
	if err != nil || got.Key != testKey {
		t.Fatalf("key not re-stored: %+v (err %v)", got, err)
	}
	form4 := url.Values{}
	form4.Set("name", "renamed")
	form4.Set("user", "u2")
	form4.Set("host", "h2")
	form4.Set("removeKey", "1")
	w = doForm(t, ui.Routes(), "/servers/"+srv.ID+"/update", form4)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("remove key got %d, want 303", w.Code)
	}
	got, err = st.Get(srv.ID)
	if err != nil || got.Key != "" {
		t.Fatalf("key not removed: %+v (err %v)", got, err)
	}

	// a key that is not a PEM block is rejected.
	bad := url.Values{}
	bad.Set("name", "renamed")
	bad.Set("user", "u2")
	bad.Set("host", "h2")
	bad.Set("key", "not-a-key")
	w = doForm(t, ui.Routes(), "/servers/"+srv.ID+"/update", bad)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad key got %d, want 400", w.Code)
	}

	// invalid (missing host) -> 400.
	form2 := url.Values{}
	form2.Set("name", "renamed")
	form2.Set("user", "u2")
	w = doForm(t, ui.Routes(), "/servers/"+srv.ID+"/update", form2)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}

	// unknown id -> 400.
	w = doForm(t, ui.Routes(), "/servers/nope/update", form)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for unknown id", w.Code)
	}
}

func TestVMPage(t *testing.T) {
	ui, st := newTestUI(t)
	srv := addServer(t, st)

	// the page shows the VM's info and its XML.
	w := doGet(t, ui.Routes(), "/servers/"+srv.ID+"/vms/debian-dev")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	body := body(w)
	for _, want := range []string{"debian-dev", "&lt;domain type=&#39;kvm&#39;&gt;", "&lt;name&gt;debian-dev&lt;/name&gt;", "id=\"vm-xml\""} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing %q", want)
		}
	}
	// actions + detail links are present
	for _, want := range []string{"/vms/debian-dev/start", "/vms/debian-dev/console", "/vms/debian-dev/shutdown"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing link %q", want)
		}
	}

	// the console-in-use badge fetches this status endpoint on load
	if !strings.Contains(body, "/console/status") {
		t.Fatalf("page missing the console status endpoint in its cfg")
	}

	// unknown VM -> 404; unknown server -> 404.
	w = doGet(t, ui.Routes(), "/servers/"+srv.ID+"/vms/ghost")
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown VM got %d, want 404", w.Code)
	}
	w = doGet(t, ui.Routes(), "/servers/nope/vms/debian-dev")
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown server got %d, want 404", w.Code)
	}
}

// TestInterruptActionsConfirmed: actions that interrupt a running VM
// (shutdown, destroy, reboot) must carry the data-confirm attribute
// (the confirm dialog); start must not.
func TestInterruptActionsConfirmed(t *testing.T) {
	ui, st := newTestUI(t)
	srv := addServer(t, st)

	// formTag returns the rendered <form ...></form> block that posts to
	// the given action, "" when absent.
	formTag := func(b, action string) string {
		i := strings.Index(b, "/vms/debian-dev/"+action+"\"")
		if i < 0 {
			return ""
		}
		j := strings.Index(b[i:], "</form>")
		if j < 0 {
			return ""
		}
		return b[i : i+j]
	}

	// the detail page carries the actions
	b := body(doGet(t, ui.Routes(), "/servers/"+srv.ID+"/vms/debian-dev"))
	for _, action := range []string{"shutdown", "destroy", "reboot"} {
		tag := formTag(b, action)
		if tag == "" {
			t.Fatalf("detail page: missing %s form", action)
		}
		if !strings.Contains(tag, "data-confirm=") {
			t.Fatalf("detail page: %s form has no data-confirm: %q", action, tag)
		}
	}
	if tag := formTag(b, "start"); tag == "" {
		t.Fatal("detail page: missing start form")
	} else if strings.Contains(tag, "data-confirm=") {
		t.Fatalf("detail page: start form unexpectedly confirmed: %q", tag)
	}

	// the listing has no actions at all — state and links only
	b = body(doGet(t, ui.Routes(), "/servers/"+srv.ID))
	for _, action := range []string{"start", "shutdown", "destroy", "reboot"} {
		if tag := formTag(b, action); tag != "" {
			t.Fatalf("listing offers %s form: %q", action, tag)
		}
	}

	// define/undefine are no longer offered in the UI
	if strings.Contains(b, "define") || strings.Contains(b, "undefine") {
		t.Fatal("detail page still offers define/undefine")
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
