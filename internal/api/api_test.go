package api

import (
	"bytes"
	"strings"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jodoherty/virtweb/internal/store"
	"github.com/jodoherty/virtweb/internal/virt"
)

type fakeBackend struct {
	mu     sync.Mutex
	actions []string
	vms    []virt.VM
	pingErr error
}

func (f *fakeBackend) record(ctx context.Context, action string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, action)
	if action == "boom" {
		return errors.New("fake action failed")
	}
	return nil
}

func (f *fakeBackend) Ping(ctx context.Context) error { return f.pingErr }
func (f *fakeBackend) ListVMs(ctx context.Context) ([]virt.VM, error) {
	return f.vms, nil
}
func (f *fakeBackend) VMInfo(ctx context.Context, name string) (virt.VM, error) {
	// vm1 has both displays (the tunnel tests resolve against it);
	// any other name is a bare VM with no display (the 503 path).
	if name == "vm1" {
		return virt.VM{Name: name, Running: true, SpiceAddr: "127.0.0.1:5991", VNCPort: 5900}, nil
	}
	return virt.VM{Name: name}, nil
}
func (f *fakeBackend) Define(ctx context.Context, xml string) error {
	name := virt.DomainName(xml)
	if name == "" {
		return errors.New("no domain name in XML")
	}
	return f.record(ctx, "define "+name)
}
func (f *fakeBackend) Undefine(ctx context.Context, name string) error {
	f.mu.Lock()
	f.actions = append(f.actions, "undefine "+name)
	f.mu.Unlock()
	if name == "boom" {
		return errors.New("fake undefine failed")
	}
	return nil
}
func (f *fakeBackend) Start(ctx context.Context, name string) error {
	return f.record(ctx, "start "+name)
}
func (f *fakeBackend) Shutdown(ctx context.Context, name string) error {
	return f.record(ctx, "shutdown "+name)
}
func (f *fakeBackend) Destroy(ctx context.Context, name string) error {
	return f.record(ctx, "destroy "+name)
}
func (f *fakeBackend) Reboot(ctx context.Context, name string) error {
	return f.record(ctx, "reboot "+name)
}
func (f *fakeBackend) Console(ctx context.Context, name string, in io.Reader, out io.Writer) error {
	f.record(ctx, "console "+name)
	// echo stdin to stdout until stdin closes, like a console session
	buf := make([]byte, 1024)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			return nil
		}
	}
}

func (f *fakeBackend) Tunnel(ctx context.Context, name, target string, in io.Reader, out io.Writer) error {
	if err := f.record(ctx, "tunnel "+name+" "+target); err != nil {
		return err
	}
	// banner in spaced chunks, then echo, like the stub backend
	for _, line := range []string{"stub-tunnel: ", name, " ", target, "\n", "chunk 1\n", "chunk 2\n"} {
		if _, werr := io.WriteString(out, line); werr != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Millisecond):
		}
	}
	buf := make([]byte, 1024)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			return nil
		}
	}
}

type fakeFactory struct {
	byID map[string]*fakeBackend
}

func (ff *fakeFactory) NewBackend(srv virt.Server) virt.Backend {
	b := ff.byID[srv.ID]
	if b == nil {
		b = &fakeBackend{vms: []virt.VM{{Name: "vm1", State: "running", Running: true}}}
		ff.byID[srv.ID] = b
	}
	return b
}

func newTestAPI(t *testing.T) (*httptest.Server, *fakeFactory, *store.Store) {
	t.Helper()
	st, err := store.New(t.TempDir() + "/servers.json")
	if err != nil {
		t.Fatal(err)
	}
	ff := &fakeFactory{byID: map[string]*fakeBackend{}}
	ts := httptest.NewServer(New(st, ff).Routes())
	t.Cleanup(ts.Close)
	return ts, ff, st
}

func TestServerCRUD(t *testing.T) {
	ts, _, _ := newTestAPI(t)

	res, err := http.Post(ts.URL+"/api/servers", "application/json",
		jsonBody(virt.Server{Name: "alpha", User: "u", Host: "h"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("add status = %d", res.StatusCode)
	}
	var created virt.Server
	json.NewDecoder(res.Body).Decode(&created)
	if created.ID == "" || created.Name != "alpha" {
		t.Fatalf("created = %+v", created)
	}

	// duplicate
	res, _ = http.Post(ts.URL+"/api/servers", "application/json",
		jsonBody(virt.Server{Name: "alpha", User: "u", Host: "h"}))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate status = %d", res.StatusCode)
	}

	// invalid (missing host)
	res, _ = http.Post(ts.URL+"/api/servers", "application/json",
		jsonBody(virt.Server{Name: "x", User: "u"}))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid status = %d", res.StatusCode)
	}

	// list
	res, _ = http.Get(ts.URL + "/api/servers")
	var list []virt.Server
	json.NewDecoder(res.Body).Decode(&list)
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list = %+v", list)
	}

	// get
	res, _ = http.Get(ts.URL + "/api/servers/" + created.ID)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", res.StatusCode)
	}
	res, _ = http.Get(ts.URL + "/api/servers/nope")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("get missing status = %d", res.StatusCode)
	}

	// remove
	req, _ := http.NewRequest("DELETE", ts.URL+"/api/servers/"+created.ID, nil)
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("remove status = %d", res.StatusCode)
	}
	res, _ = http.Get(ts.URL + "/api/servers")
	list = nil
	json.NewDecoder(res.Body).Decode(&list)
	if len(list) != 0 {
		t.Fatalf("list after remove = %+v", list)
	}
}

func TestServerUpdate(t *testing.T) {
	ts, _, _ := newTestAPI(t)

	// create
	res, err := http.Post(ts.URL+"/api/servers", "application/json",
		jsonBody(virt.Server{Name: "alpha", User: "u", Host: "h"}))
	if err != nil {
		t.Fatal(err)
	}
	var created virt.Server
	json.NewDecoder(res.Body).Decode(&created)

	put := func(body virt.Server) (*http.Response, virt.Server) {
		t.Helper()
		req, err := http.NewRequest("PUT", ts.URL+"/api/servers/"+created.ID,
			jsonBody(body))
		if err != nil {
			t.Fatal(err)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var out virt.Server
		json.NewDecoder(r.Body).Decode(&out)
		return r, out
	}

	// valid update: host + port change, id preserved.
	r, up := put(virt.Server{Name: "alpha", User: "u2", Host: "h2", Port: 2222, URI: "qemu:///system"})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d", r.StatusCode)
	}
	if up.ID != created.ID {
		t.Fatalf("id changed: %q -> %q", created.ID, up.ID)
	}
	if up.Host != "h2" || up.Port != 2222 {
		t.Fatalf("updated = %+v", up)
	}
	// reflected in GET.
	gr, _ := http.Get(ts.URL + "/api/servers/" + created.ID)
	var got virt.Server
	json.NewDecoder(gr.Body).Decode(&got)
	if got.Host != "h2" || got.Port != 2222 {
		t.Fatalf("get after update = %+v", got)
	}

	// invalid (missing host) -> 400.
	r, _ = put(virt.Server{Name: "alpha", User: "u"})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid update status = %d", r.StatusCode)
	}

	// unknown id -> 404.
	req, _ := http.NewRequest("PUT", ts.URL+"/api/servers/nope",
		jsonBody(virt.Server{Name: "x", User: "u", Host: "h"}))
	res, _ = http.DefaultClient.Do(req)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("update unknown id status = %d, want 404", res.StatusCode)
	}
}

func TestDefineVM(t *testing.T) {
	ts, ff, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	do := func(body string) (int, map[string]any) {
		t.Helper()
		res, err := http.Post(ts.URL+"/api/servers/"+id+"/vms",
			"application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}

	// plain define -> 201, name reported.
	code, out := do(`{"xml":"<domain type='kvm'><name>newvm</name></domain>"}`)
	if code != http.StatusCreated || out["name"] != "newvm" {
		t.Fatalf("define = %d %+v", code, out)
	}

	// define + start -> 201 with started.
	code, out = do(`{"xml":"<domain type='kvm'><name>newvm2</name></domain>","start":true}`)
	if code != http.StatusCreated || out["started"] != true {
		t.Fatalf("define+start = %d %+v", code, out)
	}

	// no <name> -> 400.
	code, _ = do(`{"xml":"<domain type='kvm'></domain>"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("nameless define = %d, want 400", code)
	}
	// invalid JSON -> 400.
	code, _ = do(`{`)
	if code != http.StatusBadRequest {
		t.Fatalf("bad JSON = %d, want 400", code)
	}

	// unknown server -> 404.
	res, _ := http.Post(ts.URL+"/api/servers/nope/vms", "application/json",
		strings.NewReader(`{"xml":"<domain><name>x</name></domain>"}`))
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("define on unknown server = %d, want 404", res.StatusCode)
	}

	// the fake backend recorded both defines.
	acts := append([]string(nil), ff.byID[id].actions...)
	want := map[string]bool{"define newvm": true, "define newvm2": true}
	for _, a := range acts {
		delete(want, a)
	}
	if len(want) != 0 {
		t.Fatalf("missing recorded defines: %v (all: %v)", want, acts)
	}
}

func TestUndefineVM(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	id := addTestServer(t, ts)

	del := func(name string) int {
		t.Helper()
		req, err := http.NewRequest("DELETE",
			ts.URL+"/api/servers/"+id+"/vms/"+name, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	// the fake backend records any undefine.
	if code := del("newvm"); code != http.StatusNoContent {
		t.Fatalf("undefine status = %d, want 204", code)
	}
	// "boom" makes the fake action fail -> 502.
	if code := del("boom"); code != http.StatusBadGateway {
		t.Fatalf("failing undefine status = %d, want 502", code)
	}
	// unknown server -> 404.
	req, _ := http.NewRequest("DELETE", ts.URL+"/api/servers/nope/vms/x", nil)
	res, _ := http.DefaultClient.Do(req)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("undefine on unknown server = %d, want 404", res.StatusCode)
	}
}

func TestVMListAndActions(t *testing.T) {
	ts, ff, _ := newTestAPI(t)
	res, _ := http.Post(ts.URL+"/api/servers", "application/json",
		jsonBody(virt.Server{Name: "alpha", User: "u", Host: "h"}))
	var srv virt.Server
	json.NewDecoder(res.Body).Decode(&srv)

	res, _ = http.Get(ts.URL + "/api/servers/" + srv.ID + "/vms")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("vms status = %d", res.StatusCode)
	}
	var vms []virt.VM
	json.NewDecoder(res.Body).Decode(&vms)
	if len(vms) != 1 || vms[0].Name != "vm1" {
		t.Fatalf("vms = %+v", vms)
	}

	for _, action := range []string{"start", "shutdown", "destroy", "reboot"} {
		req, _ := http.NewRequest("POST",
			ts.URL+"/api/servers/"+srv.ID+"/vms/vm1/"+action, nil)
		res, _ = http.DefaultClient.Do(req)
		if res.StatusCode != http.StatusNoContent {
			t.Fatalf("%s status = %d", action, res.StatusCode)
		}
	}
	res, _ = http.Post(ts.URL+"/api/servers/"+srv.ID+"/vms/vm1/wrongaction",
		"application/json", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown action status = %d", res.StatusCode)
	}

	backend := ff.byID[srv.ID]
	backend.mu.Lock()
	actions := append([]string(nil), backend.actions...)
	backend.mu.Unlock()
	if len(actions) != 4 {
		t.Fatalf("actions = %v", actions)
	}
}

func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}
