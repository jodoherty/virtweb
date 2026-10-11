package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jodoherty/virtweb/internal/store"
	"github.com/jodoherty/virtweb/internal/ws"
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
// refused (429 pre-upgrade) while the first max stay open.
func TestTunnelSessionLimit(t *testing.T) {
	ts, id := newAPI(t, 4, 2)
	stream := fmt.Sprintf("%s/api/servers/%s/vms/vm1/tunnel/stream?display=spice", ts.URL, id)

	open := make([]*ws.Session, 0, 2)
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		s, err := ws.Dial(ctx, wsURL(stream))
		cancel()
		if i < 2 {
			if err != nil {
				t.Fatalf("session %d dial: %v, want open", i, err)
			}
			open = append(open, s)
		} else if err == nil {
			s.Close()
			t.Fatalf("session %d dialed, want a 429 rejection", i)
		}
	}
	for _, s := range open {
		s.Close()
	}
}
// TestConsoleSessionLimit: same for consoles, across distinct
// (server, VM) keys.
func TestConsoleSessionLimit(t *testing.T) {
	ts, id := newAPI(t, 2, 4)

	open := make([]*ws.Session, 0, 2)
	for i, name := range []string{"vm1", "vm2", "vm3"} {
		stream := fmt.Sprintf("%s/api/servers/%s/vms/%s/console/stream", ts.URL, id, name)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		s, err := ws.Dial(ctx, wsURL(stream))
		cancel()
		if i < 2 {
			if err != nil {
				t.Fatalf("console %s dial: %v, want open", name, err)
			}
			open = append(open, s)
		} else if err == nil {
			s.Close()
			t.Fatalf("console %s dialed, want a 429 rejection", name)
		}
	}
	for _, s := range open {
		s.Close()
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
