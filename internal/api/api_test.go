package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"git.theodohertyfamily.com/virtweb/internal/store"
	"git.theodohertyfamily.com/virtweb/internal/virt"
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
	return virt.VM{Name: name}, nil
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
	return f.record(ctx, "console "+name)
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
