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
	streamURL := ts.URL + fmt.Sprintf("/api/servers/%s/vms/vm1/tunnel/stream?port=5991", id)
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
	want := "stub-tunnel: vm1 h:5991\nchunk 1\nchunk 2\nhello"
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
	streamURL := ts.URL + fmt.Sprintf("/api/servers/%s/vms/vm1/tunnel/stream?port=5991", id)

	a, sidA := openTunnelStream(t, client, streamURL)
	b, sidB := openTunnelStream(t, client, streamURL)
	if sidA == sidB {
		t.Fatalf("concurrent same-target tunnels got the same id %q", sidA)
	}
	a.Body.Close()
	b.Body.Close()
}

func TestTunnelTargetValidation(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	id := addTestServer(t, ts)

	for _, q := range []string{"?port=abc", "?port=0", "?port=99999", "?target=5991", ""} {
		res, err := http.Get(ts.URL + fmt.Sprintf("/api/servers/%s/vms/vm1/tunnel/stream%s", id, q))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("stream %q status = %d, want 400", q, res.StatusCode)
		}
	}

	// Explicit host:port target works.
	res, err := http.Get(ts.URL + fmt.Sprintf("/api/servers/%s/vms/vm1/tunnel/stream?target=192.0.2.10:5991", id))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("explicit target status = %d, want 200", res.StatusCode)
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
