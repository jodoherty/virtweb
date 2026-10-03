package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jodoherty/virtweb/internal/virt"
)

// Generic TCP tunnel over plain HTTP — the same shape as the serial
// console (chunked flushed GET + small input POSTs, one pipeSession
// per session, no WebSocket), but for any TCP service reachable from
// the remote host: SPICE (5991), VNC (59xx), ...
//
// A stream GET opens a fresh tunnel session and returns its handle in
// the X-Tunnel-Id response header; input POSTs reference that handle
// (?id=<sid>). The key is the session id — NOT (server, VM, target) —
// because a single SPICE session opens several channels (main, display,
// inputs, cursor) that each need their own concurrent tunnel to the
// same target. Two concurrent streams to the same target are
// therefore both valid and get distinct ids.
//
// The client selects the remote endpoint with ?target=host:port or
// ?port=N (target defaults to the server host itself, where the
// hypervisor's display servers listen). r.Context() cancels on client
// disconnect and is passed to the backend, which kills the ssh session.

// maxTunnelInputBody caps a single input POST. SPICE/VNC client→server
// messages are small, but display-adjacent traffic (agent, clipboard)
// can get bigger than keystrokes.
const maxTunnelInputBody = 512 << 10

// errTunnelNoID is the input POST missing its ?id= session handle.
var errTunnelNoID = errors.New("missing tunnel session id")
var errTunnelTarget = errors.New("invalid tunnel target")

// newTunnelID returns a random hex session handle.
func newTunnelID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on any supported platform; fall
		// back to a time-seeded counter would be less unique, so just
		// report a zero id and let the caller retry on collision.
		return strings.Repeat("0", 16)
	}
	return hex.EncodeToString(b)
}

// tunnelTarget resolves the ?target=/ ?port= query into "host:port".
func tunnelTarget(srv virt.Server, q url.Values) (string, error) {
	if t := q.Get("target"); t != "" {
		if strings.ContainsAny(t, " \t") || !strings.Contains(t, ":") {
			return "", errTunnelTarget
		}
		return t, nil
	}
	p, err := strconv.Atoi(q.Get("port"))
	if err != nil || p < 1 || p > 65535 {
		return "", errTunnelTarget
	}
	return srv.Host + ":" + strconv.Itoa(p), nil
}

// handleTunnelStream opens a new tunnel session: a chunked, flushed
// binary response the browser consumes as a ReadableStream. The
// session handle is delivered in the X-Tunnel-Id header so the client
// can route its input POSTs here.
func (a *API) handleTunnelStream(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("name")
	srv, b, err := a.backend(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}
	target, err := tunnelTarget(srv, r.URL.Query())
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v: use ?target=host:port or ?port=N", errTunnelTarget)
		return
	}
	sid := newTunnelID()
	s := newPipeSession()
	a.tunMu.Lock()
	a.tunnels[sid] = s
	a.tunMu.Unlock()
	defer a.dropTunnel(sid)

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// Reverse proxies must pass the stream through unbuffered and
	// uncompressed (h2 or not); see AGENTS.md.
	w.Header().Set("X-Accel-Buffering", "no")
	// The session handle for input POSTs.
	w.Header().Set("X-Tunnel-Id", sid)
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}

	// Run the tunnel; when it ends (remote end hung up, ssh error, or
	// the client disconnected and r.Context() cancelled it), close the
	// pipes so the pump below terminates.
	go func() {
		err := b.Tunnel(r.Context(), name, target, s.inRead, s.outWrite)
		s.close()
		if err != nil {
			log.Printf("tunnel %s %s: %v", name, target, err)
		}
	}()

	buf := make([]byte, 32*1024)
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for {
			n, err := s.outRead.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-r.Context().Done(): // client disconnected
	case <-pumpDone:          // tunnel output ended
	}
	s.close()
	// The pump goroutine may still be inside Write/Flush; the response
	// writer must not be used concurrently with the server finishing
	// the response, so wait for it to fully drain before returning.
	<-pumpDone
}

// handleTunnelInput writes one input chunk to the tunnel session
// named by ?id=<sid>. The body is raw bytes (application/octet-stream).
func (a *API) handleTunnelInput(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("id")
	if sid == "" {
		writeErr(w, http.StatusBadRequest, "%v: input POSTs need ?id=<session id>", errTunnelNoID)
		return
	}
	a.tunMu.Lock()
	s := a.tunnels[sid]
	a.tunMu.Unlock()
	if s == nil {
		writeErr(w, http.StatusNotFound, "no tunnel session %q open", sid)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxTunnelInputBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read input: %v", err)
		return
	}
	if err := s.write(body); err != nil {
		writeErr(w, http.StatusNotFound, "tunnel session %q is not open", sid)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// dropTunnel removes a finished session from the registry.
func (a *API) dropTunnel(sid string) {
	a.tunMu.Lock()
	defer a.tunMu.Unlock()
	delete(a.tunnels, sid)
}
