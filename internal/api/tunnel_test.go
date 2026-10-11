package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jodoherty/virtweb/internal/virt"
	"github.com/jodoherty/virtweb/internal/ws"
)

// tunnelURL is the tunnel stream URL for (server id, VM, display).
func tunnelURL(t *testing.T, base, id, name, display string) string {
	t.Helper()
	return base + fmt.Sprintf("/api/servers/%s/vms/%s/tunnel/stream?display=%s", id, name, display)
}

// readUntil reads binary messages until one carries want (or the
// bound is hit) and returns the concatenation of everything read.
// The tunnel backend's banner arrives in spaced chunks, so an echo
// assertion has to read past it rather than assume message boundaries.
func readUntil(t *testing.T, s *ws.Session, want string) string {
	t.Helper()
	var got []byte
	for i := 0; i < 64; i++ {
		_, data, err := s.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		got = append(got, data...)
		if strings.Contains(string(got), want) {
			return string(got)
		}
	}
	t.Fatalf("did not observe %q within 64 messages", want)
	return ""
}

// TestTunnelStreamRoundTrip: dial the VNC tunnel, bytes round-trip
// through the echo backend, and the session closes cleanly.
func TestTunnelStreamRoundTrip(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	id := addTestServer(t, ts)

	s := dialStream(t, tunnelURL(t, ts.URL, id, "vm1", "vnc"))
	if err := s.WriteBinary([]byte("rfb-head")); err != nil {
		t.Fatal(err)
	}
	got := readUntil(t, s, "rfb-head")
	if !strings.Contains(got, "stub-tunnel:") {
		t.Fatalf("stream = %q, want the session banner before the echo", got)
	}
	if err := s.Shutdown(1000, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReadMessage(); !errors.Is(err, io.EOF) {
		t.Fatalf("clean-end read = %v, want io.EOF", err)
	}
	s.Close()
}

// TestTunnelConcurrentSameTarget: several concurrent sessions to the
// same target are valid and independent (a SPICE session opens one
// tunnel per channel). The two sessions' bytes never cross.
func TestTunnelConcurrentSameTarget(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	url := tunnelURL(t, ts.URL, id, "vm1", "spice")

	a := dialStream(t, url)
	defer a.Close()
	b := dialStream(t, url)
	defer b.Close()

	if err := a.WriteBinary([]byte("A")); err != nil {
		t.Fatal(err)
	}
	if err := b.WriteBinary([]byte("B")); err != nil {
		t.Fatal(err)
	}
	if got := readUntil(t, a, "A"); !strings.Contains(got, "A") {
		t.Fatalf("session A stream = %q, want its echo", got)
	}
	if got := readUntil(t, b, "B"); !strings.Contains(got, "B") {
		t.Fatalf("session B stream = %q, want its echo", got)
	}
}

// TestTunnelStreamTerminalError: a session that fails server-side ends
// with closeSessionError and the server's message.
func TestTunnelStreamTerminalError(t *testing.T) {
	ts, ff, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	ff.byID[id] = &fakeBackend{
		vms:       []virt.VM{{Name: "vm1", Running: true, VNCPort: 5900}},
		tunnelErr: errors.New("connection refused"),
	}

	s := dialStream(t, tunnelURL(t, ts.URL, id, "vm1", "vnc"))
	_, _, err := s.ReadMessage()
	var closeErr *ws.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("terminal read = %v, want *ws.CloseError", err)
	}
	if closeErr.Code != closeSessionError || closeErr.Reason != "connection refused" {
		t.Fatalf("terminal = %d %q, want %d with the message", closeErr.Code, closeErr.Reason, closeSessionError)
	}
	s.Close()
}

// TestTunnelStreamTerminalCleanEnd: a session that ends cleanly
// delivers its final output, then closes with 1000.
func TestTunnelStreamTerminalCleanEnd(t *testing.T) {
	ts, ff, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	ff.byID[id] = &fakeBackend{
		vms:         []virt.VM{{Name: "vm1", Running: true, VNCPort: 5900}},
		tunnelClean: true,
	}

	s := dialStream(t, tunnelURL(t, ts.URL, id, "vm1", "vnc"))
	op, data, err := s.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if op != ws.OpBinary || string(data) != "stub-tunnel: vm1\n" {
		t.Fatalf("data = %q (op %d), want the tunnel banner", data, op)
	}
	if _, _, err = s.ReadMessage(); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal read = %v, want io.EOF (clean 1000 end)", err)
	}
	s.Close()
}

// TestTunnelDisplayValidation: the display is resolved server-side; a
// bogus or missing display and a VM with no display are pre-upgrade
// rejections (the dial fails).
func TestTunnelDisplayValidation(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	dial := func(name, display string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := ws.Dial(ctx, wsURL(tunnelURL(t, ts.URL, id, name, display)))
		if err == nil {
			t.Fatalf("tunnel %s display=%q dialed, want a rejection", name, display)
		}
	}
	dial("vm1", "bogus") // 400
	dial("vm2", "vnc")   // vm2 has no display: 503
	dial("vm2", "spice")
	// The preflight surfaces the same statuses without opening a session.
	if code := preflight(t, tunnelURL(t, ts.URL, id, "vm1", "bogus")); code != http.StatusBadRequest {
		t.Fatalf("preflight bogus display = %d, want 400", code)
	}
	if code := preflight(t, tunnelURL(t, ts.URL, id, "vm2", "vnc")); code != http.StatusServiceUnavailable {
		t.Fatalf("preflight missing display = %d, want 503", code)
	}
	if code := preflight(t, tunnelURL(t, ts.URL, id, "vm1", "vnc")); code != http.StatusOK {
		t.Fatalf("preflight ok display = %d, want 200", code)
	}
}

// TestTunnelStreamUnknownServer: dialing a tunnel for a server that
// does not exist fails (404 pre-upgrade).
func TestTunnelStreamUnknownServer(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := ws.Dial(ctx, wsURL(tunnelURL(t, ts.URL, "nope", "vm1", "vnc"))); err == nil {
		t.Fatal("tunnel dial for an unknown server succeeded, want a 404 rejection")
	}
}
