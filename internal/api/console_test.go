package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jodoherty/virtweb/internal/virt"
)

// addTestServer registers a server and returns its ID.
func addTestServer(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	res, err := http.Post(ts.URL+"/api/servers", "application/json",
		jsonBody(virt.Server{Name: "alpha", User: "u", Host: "h"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("add status = %d", res.StatusCode)
	}
	var srv virt.Server
	json.NewDecoder(res.Body).Decode(&srv)
	return srv.ID
}

func TestConsoleStreamRoundTrip(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	streamURL := ts.URL + fmt.Sprintf("/api/servers/%s/vms/vm1/console/stream", id)

	client := &http.Client{}
	res, err := client.Get(streamURL)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", res.StatusCode)
	}

	// A second concurrent stream for the same VM must be rejected.
	other, err := client.Get(streamURL)
	if err != nil {
		t.Fatal(err)
	}
	other.Body.Close()
	if other.StatusCode != http.StatusConflict {
		t.Fatalf("second stream status = %d, want 409", other.StatusCode)
	}

	// Read the stream in the background; it ends when the client
	// connection (below) is closed. The final read errors because we
	// close the connection ourselves — that is fine, only the bytes
	// received matter.
	out := make(chan []byte, 1)
	go func() {
		buf, err := io.ReadAll(res.Body)
		if err != nil && len(buf) == 0 {
			t.Errorf("read stream: %v", err)
		}
		out <- buf
	}()

	// Send input: fakeBackend.Console echoes stdin to stdout.
	inRes, err := client.Post(ts.URL+fmt.Sprintf("/api/servers/%s/vms/vm1/console/input", id),
		"application/octet-stream", strings.NewReader("ping"))
	if err != nil {
		t.Fatal(err)
	}
	if inRes.StatusCode != http.StatusNoContent {
		t.Fatalf("input status = %d", inRes.StatusCode)
	}

	// Closing the client stream disconnects the request, which cancels
	// the backend context and ends the session.
	res.Body.Close()
	buf := <-out
	if string(buf) != "ping" {
		t.Fatalf("streamed output = %q, want %q", buf, "ping")
	}
}

func TestConsoleInputWithoutSession(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	res, err := http.Post(ts.URL+fmt.Sprintf("/api/servers/%s/vms/vm1/console/input", id),
		"application/octet-stream", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("input status = %d, want 404", res.StatusCode)
	}
}

func TestConsoleStreamUnknownServer(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	res, err := http.Get(ts.URL + "/api/servers/nope/vms/vm1/console/stream")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("stream status = %d, want 404", res.StatusCode)
	}
}
