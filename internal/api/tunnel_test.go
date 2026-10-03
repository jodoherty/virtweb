package api

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// openTunnelStream opens a tunnel stream and returns the response plus
// its session id (X-Tunnel-Id).
func openTunnelStream(t *testing.T, client *http.Client, streamURL string) (*http.Response, string) {
	t.Helper()
	res, err := client.Get(streamURL)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", res.StatusCode)
	}
	sid := res.Header.Get("X-Tunnel-Id")
	if sid == "" {
		t.Fatalf("stream response missing X-Tunnel-Id header")
	}
	return res, sid
}

func TestTunnelStreamRoundTrip(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	client := &http.Client{}
	streamURL := ts.URL + fmt.Sprintf("/api/servers/%s/vms/vm1/tunnel/stream?display=spice", id)
	inputURL := ts.URL + fmt.Sprintf("/api/servers/%s/vms/vm1/tunnel/input", id)

	res, sid := openTunnelStream(t, client, streamURL)
	if res.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", res.Header.Get("X-Accel-Buffering"))
	}
	if res.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", res.Header.Get("Cache-Control"))
	}

	out := make(chan []byte, 1)
	go func() {
		buf, err := io.ReadAll(res.Body)
		if err != nil && len(buf) == 0 {
			t.Errorf("read stream: %v", err)
		}
		out <- buf
	}()

	// Input is routed by session id and echoes back through the tunnel.
	inRes, err := client.Post(inputURL+"?id="+sid,
		"application/octet-stream", strings.NewReader("hello"))
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
	// The destination is resolved server-side: vm1's SPICE display is
	// 127.0.0.1:5991 in the fake backend.
	want := "stub-tunnel: vm1 127.0.0.1:5991\nchunk 1\nchunk 2\nhello"
	if string(buf) != want {
		t.Fatalf("streamed output = %q, want %q", buf, want)
	}
}

// A single SPICE session opens several channels that each need their
// own concurrent tunnel to the same target; both must succeed with
// distinct session ids.
func TestTunnelConcurrentSameTarget(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	client := &http.Client{}
	streamURL := ts.URL + fmt.Sprintf("/api/servers/%s/vms/vm1/tunnel/stream?display=spice", id)

	a, sidA := openTunnelStream(t, client, streamURL)
	b, sidB := openTunnelStream(t, client, streamURL)
	if sidA == sidB {
		t.Fatalf("concurrent same-target tunnels got the same id %q", sidA)
	}
	a.Body.Close()
	b.Body.Close()
}

func TestTunnelDisplayValidation(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	stream := func(name, q string) int {
		res, err := http.Get(ts.URL + fmt.Sprintf("/api/servers/%s/vms/%s/tunnel/stream%s", id, name, q))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	// Missing or invalid display -> 400.
	for _, q := range []string{"", "?display=bogus", "?display="} {
		if got := stream("vm1", q); got != http.StatusBadRequest {
			t.Fatalf("stream %q status = %d, want 400", q, got)
		}
	}

	// A client-supplied ?target= is ignored; only ?display= is honoured.
	// (vm1's spice display resolves to 127.0.0.1:5991, not the target.)
	if got := stream("vm1", "?display=spice&target=1.2.3.4:9999"); got != http.StatusOK {
		t.Fatalf("stream with ignored target status = %d, want 200", got)
	}

	// Both displays resolve on vm1.
	if got := stream("vm1", "?display=vnc"); got != http.StatusOK {
		t.Fatalf("stream ?display=vnc status = %d, want 200", got)
	}
	if got := stream("vm1", "?display=spice"); got != http.StatusOK {
		t.Fatalf("stream ?display=spice status = %d, want 200", got)
	}

	// A VM with no such display -> 503 (the requested display is not
	// enabled).
	if got := stream("vm2", "?display=spice"); got != http.StatusServiceUnavailable {
		t.Fatalf("stream vm2 ?display=spice status = %d, want 503", got)
	}
}

func TestTunnelInputRouting(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	inputURL := ts.URL + fmt.Sprintf("/api/servers/%s/vms/vm1/tunnel/input", id)

	// Missing ?id= -> 400.
	res, err := http.Post(inputURL, "application/octet-stream", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("input without id status = %d, want 400", res.StatusCode)
	}

	// Unknown id -> 404.
	res, err = http.Post(inputURL+"?id=nope", "application/octet-stream", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("input with unknown id status = %d, want 404", res.StatusCode)
	}
}
