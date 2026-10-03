package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

	// Send input as a DATA frame: fakeBackend.Console echoes stdin to stdout.
	inRes, err := client.Post(ts.URL+fmt.Sprintf("/api/servers/%s/vms/vm1/console/input", id),
		"application/octet-stream",
		bytes.NewReader(encodeTestFrame(t, frameData, []byte("ping"))))
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
	frames, rest, err := framesFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 0 || len(frames) != 2 {
		t.Fatalf("streamed frames = %+v rest=%d, want HELLO + DATA", frames, len(rest))
	}
	if frames[0].tag != frameHello ||
		!bytes.Equal(frames[0].payload, []byte{frameVersion, helloFlags}) {
		t.Fatalf("first frame = %+v, want HELLO", frames[0])
	}
	if frames[1].tag != frameData || string(frames[1].payload) != "ping" {
		t.Fatalf("second frame = %+v, want DATA ping", frames[1])
	}
}

// TestConsoleStreamTerminalError: a session that fails server-side
// (the backend call returns an error) ends with a SESSION_ERROR frame
// carrying the server's message, then the stream closes cleanly.
func TestConsoleStreamTerminalError(t *testing.T) {
	ts, ff, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	// Pre-seed the factory (the backend is materialized lazily on
	// first use).
	ff.byID[id] = &fakeBackend{
		vms:        []virt.VM{{Name: "vm1", State: "running", Running: true}},
		consoleErr: errors.New("domain 'vm1' is not running"),
	}

	res, err := http.Get(ts.URL + fmt.Sprintf("/api/servers/%s/vms/vm1/console/stream", id))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", res.StatusCode)
	}
	// The stream ends server-side: no client disconnect involved.
	buf, rerr := io.ReadAll(res.Body)
	if rerr != nil {
		t.Fatalf("read stream: %v", rerr)
	}
	frames, rest, ferr := framesFrom(buf)
	if ferr != nil {
		t.Fatal(ferr)
	}
	if len(rest) != 0 {
		t.Fatalf("leftover bytes: %d", len(rest))
	}
	if len(frames) != 2 {
		t.Fatalf("frames = %+v, want HELLO + SESSION_ERROR", frames)
	}
	if frames[1].tag != frameError ||
		string(frames[1].payload) != "domain 'vm1' is not running" {
		t.Fatalf("terminal frame = %+v, want SESSION_ERROR with the message", frames[1])
	}
}

// TestConsoleStreamTerminalCleanEnd: a session that ends cleanly
// (the backend returns nil) ends with a SESSION_END frame as the last
// frame.
func TestConsoleStreamTerminalCleanEnd(t *testing.T) {
	ts, ff, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	ff.byID[id] = &fakeBackend{
		vms:          []virt.VM{{Name: "vm1", State: "running", Running: true}},
		consoleClean: true,
	}

	res, err := http.Get(ts.URL + fmt.Sprintf("/api/servers/%s/vms/vm1/console/stream", id))
	if err != nil {
		t.Fatal(err)
	}
	buf, rerr := io.ReadAll(res.Body)
	if rerr != nil {
		t.Fatalf("read stream: %v", rerr)
	}
	frames, rest, ferr := framesFrom(buf)
	if ferr != nil {
		t.Fatal(ferr)
	}
	if len(rest) != 0 || len(frames) != 3 {
		t.Fatalf("frames = %+v rest=%d, want HELLO + DATA + SESSION_END", frames, len(rest))
	}
	if frames[1].tag != frameData || string(frames[1].payload) != "banner\n" {
		t.Fatalf("data frame = %+v, want DATA banner", frames[1])
	}
	if frames[2].tag != frameEnd || len(frames[2].payload) != 0 {
		t.Fatalf("terminal frame = %+v, want SESSION_END (empty)", frames[2])
	}
}

// TestConsoleStatus: the console-in-use indicator — closed until a
// session is open, open while it is, closed again after the client
// goes away (with the session settled).
func TestConsoleStatus(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	statusURL := ts.URL + fmt.Sprintf("/api/servers/%s/vms/vm1/console/status", id)

	res, err := http.Get(statusURL)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var out map[string]bool
	json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	if out["open"] {
		t.Fatalf("open = %v, want false", out)
	}

	// open a session
	client := &http.Client{}
	stream, err := client.Get(ts.URL + fmt.Sprintf("/api/servers/%s/vms/vm1/console/stream", id))
	if err != nil {
		t.Fatal(err)
	}
	res, _ = http.Get(statusURL)
	json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	if !out["open"] {
		t.Fatalf("open = %v, want true while the stream is live", out)
	}

	// client goes away; wait for the server to drop the session
	stream.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		res, _ = http.Get(statusURL)
		json.NewDecoder(res.Body).Decode(&out)
		res.Body.Close()
		if !out["open"] {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if out["open"] {
		t.Fatalf("open = %v, want false after the stream closed", out)
	}

	// unknown server -> 404
	res, _ = http.Get(ts.URL + "/api/servers/nope/vms/x/console/status")
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown server status = %d, want 404", res.StatusCode)
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
