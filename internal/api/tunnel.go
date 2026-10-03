package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
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
// The client names which display it wants to view (?display=vnc|
// spice); the server resolves the destination from the VM's live
// display info (virsh domdisplay / graphics port). No client-supplied
// host:port is accepted — that would let an authenticated user point
// the remote SSH user at an arbitrary TCP service (SSRF: port
// scanning, cloud metadata, services on the remote host's loopback).
// r.Context() cancels on client disconnect and is passed to the
// backend, which kills the ssh session.

// maxTunnelInputBody caps a single input POST. SPICE/VNC client→server
// messages are small, but display-adjacent traffic (agent, clipboard)
// can get bigger than keystrokes.
const maxTunnelInputBody = 512 << 10

var (
	errTunnelNoID = errors.New("missing tunnel session id")
	errBadDisplay = errors.New("invalid or missing ?display= (want vnc or spice)")
	errNoDisplay  = errors.New("the requested display is not enabled on this VM")
)

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

// displayTarget resolves the tunnel destination server-side from the
// VM's live display info. The client picks which display to view; the
// server decides where that display actually lives. There is
// deliberately no client-supplied host:port (see the package comment).
func displayTarget(srv virt.Server, vm virt.VM, display string) (string, error) {
	switch display {
	case "vnc":
		if vm.VNCPort <= 0 {
			return "", errNoDisplay
		}
		return srv.Host + ":" + strconv.Itoa(vm.VNCPort), nil
	case "spice":
		if vm.SpiceAddr == "" {
			return "", errNoDisplay
		}
		return vm.SpiceAddr, nil
	default:
		return "", errBadDisplay
	}
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
	display := r.URL.Query().Get("display")
	if display != "vnc" && display != "spice" {
		writeErr(w, http.StatusBadRequest, "%v", errBadDisplay)
		return
	}
	vm, err := b.VMInfo(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "display info: %v", err)
		return
	}
	target, err := displayTarget(srv, vm, display)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "%v", err)
		return
	}
	sid := newTunnelID()
	s := newPipeSession()
	a.tunMu.Lock()
	if len(a.tunnels) >= a.maxTunnels {
		a.tunMu.Unlock()
		writeErr(w, http.StatusTooManyRequests,
			"too many tunnel sessions open (limit %d); try again shortly", a.maxTunnels)
		return
	}
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
	// Announce the wire protocol (v1) before any tunnel bytes; the
	// client fails fast on a non-matching stream (see frame.go).
	if err := writeHello(w, flusher); err != nil {
		log.Printf("tunnel %s %s: hello: %v", name, target, err)
		return
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

	pumpDone := make(chan struct{})
	go pumpFramed(s.outRead, w, a.keepalive, pumpDone)
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
// named by ?id=<sid>. The body is one wire-protocol frame (usually a
// DATA frame carrying the display bytes, or a KEEPALIVE from an idle
// client).
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
	tag, payload, err := readFrameBody(r, maxTunnelInputBody)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "input frame: %v", err)
		return
	}
	switch tag {
	case frameKeepalive:
		// A liveness signal from an idle client; nothing to do.
	case frameData:
		if len(payload) == 0 {
			writeErr(w, http.StatusBadRequest, "empty DATA frame")
			return
		}
		if err := s.write(payload); err != nil {
			writeErr(w, http.StatusNotFound, "tunnel session %q is not open", sid)
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "unexpected frame tag 0x%02x", tag)
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
