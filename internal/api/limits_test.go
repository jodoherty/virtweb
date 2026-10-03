package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jodoherty/virtweb/internal/store"
)

// newAPI builds a test API with the session caps set explicitly (New
// defaults them to 64, which is far more sessions than a test wants
// to open).
func newAPI(t *testing.T, maxConsoles, maxTunnels int) (*httptest.Server, string) {
	t.Helper()
	st, err := store.New(t.TempDir() + "/servers.json")
	if err != nil {
		t.Fatal(err)
	}
	a := New(st, &fakeFactory{byID: map[string]*fakeBackend{}})
	a.maxConsoles = maxConsoles
	a.maxTunnels = maxTunnels
	ts := httptest.NewServer(a.Routes())
	t.Cleanup(ts.Close)
	id := addTestServer(t, ts)
	return ts, id
}

// TestTunnelSessionLimit: the (max+1)th concurrent tunnel session is
// refused with 429 while the first max stay open.
func TestTunnelSessionLimit(t *testing.T) {
	ts, id := newAPI(t, 4, 2)
	stream := fmt.Sprintf("%s/api/servers/%s/vms/vm1/tunnel/stream?display=spice", ts.URL, id)

	open := []*http.Response{}
	for i := 0; i < 3; i++ {
		res, err := http.Get(stream)
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			if res.StatusCode != http.StatusOK {
				t.Fatalf("session %d status = %d, want 200", i, res.StatusCode)
			}
			open = append(open, res)
		} else {
			if res.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("session %d status = %d, want 429", i, res.StatusCode)
			}
			res.Body.Close()
		}
	}
	for _, res := range open {
		res.Body.Close()
	}
}

// TestConsoleSessionLimit: same for consoles, across distinct
// (server, VM) keys.
func TestConsoleSessionLimit(t *testing.T) {
	ts, id := newAPI(t, 2, 4)

	open := []*http.Response{}
	for i, name := range []string{"vm1", "vm2", "vm3"} {
		res, err := http.Get(fmt.Sprintf("%s/api/servers/%s/vms/%s/console/stream", ts.URL, id, name))
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			if res.StatusCode != http.StatusOK {
				t.Fatalf("console %s status = %d, want 200", name, res.StatusCode)
			}
			open = append(open, res)
		} else {
			if res.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("console %s status = %d, want 429", name, res.StatusCode)
			}
			res.Body.Close()
		}
	}
	for _, res := range open {
		res.Body.Close()
	}
}

// TestAddServerBodyCap: an oversized JSON body is refused (400)
// instead of being read in full.
func TestAddServerBodyCap(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	res, err := http.Post(ts.URL+"/api/servers", "application/json",
		strings.NewReader(strings.Repeat("a", 2<<20))) // 2 MiB > 1 MiB cap
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized add-server status = %d, want 400", res.StatusCode)
	}
}
