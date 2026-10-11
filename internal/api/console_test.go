package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/jodoherty/virtweb/internal/virt"
	"github.com/jodoherty/virtweb/internal/ws"
)

// TestConsoleStreamRoundTrip: dial, the browser's HEAD preflight
// reports the session in use, a keystroke round-trips through the
// echo backend, the session closes cleanly (1000), and the server
// drops the session afterwards.
func TestConsoleStreamRoundTrip(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	streamURL := ts.URL + fmt.Sprintf("/api/servers/%s/vms/vm1/console/stream", id)
	statusURL := ts.URL + fmt.Sprintf("/api/servers/%s/vms/vm1/console/status", id)

	s := dialStream(t, streamURL)
	// While the session is live the preflight reports conflict.
	if code := preflight(t, streamURL); code != http.StatusConflict {
		t.Fatalf("preflight while open = %d, want 409", code)
	}
	// And the status endpoint agrees.
	if !statusOpen(t, statusURL) {
		t.Fatal("status = false, want open while the session is live")
	}

	// fakeBackend.Console echoes stdin to stdout.
	if err := s.WriteBinary([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	op, data, err := s.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if op != ws.OpBinary || string(data) != "ping\n" {
		t.Fatalf("round-trip = %q (op %d), want ping", data, op)
	}

	// Clean close: 1000 from the client; the read then reports a clean
	// end (io.EOF).
	if err := s.Shutdown(1000, "bye"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ReadMessage(); !errors.Is(err, io.EOF) {
		t.Fatalf("clean-end read = %v, want io.EOF", err)
	}
	s.Close()

	waitFor(t, 3*time.Second, func() bool {
		return preflight(t, streamURL) == http.StatusOK
	})
	if statusOpen(t, statusURL) {
		t.Fatal("session not dropped after the client closed")
	}
}

// TestConsoleStreamCrossOrigin: a browser page from a different origin
// cannot open a session (cross-site WebSocket hijacking): mTLS
// authenticates the browser's connection, not the page, so the
// handshake's Origin must be same-origin. The reject happens before the
// upgrade — no session is opened.
func TestConsoleStreamCrossOrigin(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	streamURL := ts.URL + fmt.Sprintf("/api/servers/%s/vms/vm1/console/stream", id)

	// A raw (non-upgrade-aware) client sends the handshake headers; a
	// cross-origin Origin must be refused with 403, not upgraded.
	req, _ := http.NewRequest(http.MethodGet, streamURL, nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Origin", "http://evil.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin handshake = %d, want 403", res.StatusCode)
	}
	// No session was opened.
	if got := preflight(t, streamURL); got != http.StatusOK {
		t.Fatalf("preflight = %d, want 200 (no session opened)", got)
	}
}

// TestConsoleStreamConflict: a second concurrent session for the same
// (server, VM) is refused — the dial fails (409 pre-upgrade) and the
// preflight reports it.
func TestConsoleStreamConflict(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	streamURL := ts.URL + fmt.Sprintf("/api/servers/%s/vms/vm1/console/stream", id)

	first := dialStream(t, streamURL)
	defer first.Close()

	if code := preflight(t, streamURL); code != http.StatusConflict {
		t.Fatalf("preflight = %d, want 409", code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := ws.Dial(ctx, wsURL(streamURL)); err == nil {
		t.Fatal("second concurrent session dial succeeded, want a 409 rejection")
	}
}

// TestConsoleStreamTerminalError: a session that fails server-side
// ends with close code closeSessionError and the server's message as
// the close reason.
func TestConsoleStreamTerminalError(t *testing.T) {
	ts, ff, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	// Pre-seed the factory (the backend is materialized lazily on
	// first use).
	ff.byID[id] = &fakeBackend{
		vms:        []virt.VM{{Name: "vm1", State: "running", Running: true}},
		consoleErr: errors.New("domain 'vm1' is not running"),
	}

	s := dialStream(t, ts.URL+fmt.Sprintf("/api/servers/%s/vms/vm1/console/stream", id))
	_, _, err := s.ReadMessage()
	var closeErr *ws.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("terminal read = %v, want *ws.CloseError", err)
	}
	if closeErr.Code != closeSessionError || closeErr.Reason != "domain 'vm1' is not running" {
		t.Fatalf("terminal = %d %q, want %d with the message", closeErr.Code, closeErr.Reason, closeSessionError)
	}
	s.Close()
}

// TestConsoleStreamTerminalCleanEnd: a session that ends cleanly
// (the backend returns nil) delivers its final output, then closes
// with 1000 — a clean end the client observes as io.EOF.
func TestConsoleStreamTerminalCleanEnd(t *testing.T) {
	ts, ff, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	ff.byID[id] = &fakeBackend{
		vms:          []virt.VM{{Name: "vm1", State: "running", Running: true}},
		consoleClean: true,
	}

	s := dialStream(t, ts.URL+fmt.Sprintf("/api/servers/%s/vms/vm1/console/stream", id))
	op, data, err := s.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if op != ws.OpBinary || string(data) != "banner\n" {
		t.Fatalf("data = %q (op %d), want banner", data, op)
	}
	if _, _, err = s.ReadMessage(); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal read = %v, want io.EOF (clean 1000 end)", err)
	}
	s.Close()
}

// TestConsoleStatus: the console-in-use indicator — closed until a
// session is open, open while it is, closed again after the client
// goes away (with the session settled).
func TestConsoleStatus(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	streamURL := ts.URL + fmt.Sprintf("/api/servers/%s/vms/vm1/console/stream", id)
	statusURL := ts.URL + fmt.Sprintf("/api/servers/%s/vms/vm1/console/status", id)

	if statusOpen(t, statusURL) {
		t.Fatal("open = true, want false before any session")
	}

	s := dialStream(t, streamURL)
	if !statusOpen(t, statusURL) {
		t.Fatal("open = false, want true while the session is live")
	}

	// Client goes away (no polite close): the server notices the
	// transport died and drops the session.
	s.Close()
	waitFor(t, 3*time.Second, func() bool { return !statusOpen(t, statusURL) })
	if statusOpen(t, statusURL) {
		t.Fatal("session not dropped after the client went away")
	}

	// Unknown server -> 404.
	res, err := http.Get(ts.URL + "/api/servers/nope/vms/x/console/status")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown server status = %d, want 404", res.StatusCode)
	}
}

// TestConsoleStreamUnknownServer: dialing a session for a server that
// does not exist fails (404 pre-upgrade).
func TestConsoleStreamUnknownServer(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := ws.Dial(ctx, wsURL(ts.URL+"/api/servers/nope/vms/vm1/console/stream")); err == nil {
		t.Fatal("dial for an unknown server succeeded, want a 404 rejection")
	}
}

func statusOpen(t *testing.T, statusURL string) bool {
	t.Helper()
	res, err := http.Get(statusURL)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	defer res.Body.Close()
	var out map[string]bool
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("status body: %v", err)
	}
	return out["open"]
}

// waitFor polls fn until it reports true or the deadline passes.
func waitFor(t *testing.T, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting: " + t.Name())
}
