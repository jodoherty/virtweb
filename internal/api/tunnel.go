package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/jodoherty/virtweb/internal/virt"
)

// Generic TCP tunnel over WebSocket — one session per channel (see
// stream.go for the transport). A SPICE session opens several
// channels (main, display, inputs, cursor, playback), each its own
// concurrent tunnel to the same target, so several sessions per
// (server, VM) are both valid and expected; the session registry
// bounds the total (429 when reached).
//
// The client names which display it wants to view (?display=vnc|
// spice); the server resolves the destination from the VM's live
// display info (virsh domdisplay / graphics port). No client-supplied
// host:port is accepted — that would let an authenticated user point
// the remote SSH user at an arbitrary TCP service (SSRF: port
// scanning, cloud metadata, services on the remote host's loopback).

// newTunnelID returns a random hex session handle (registry key).
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

var (
	errBadDisplay = errors.New("invalid or missing ?display= (want vnc or spice)")
	errNoDisplay  = errors.New("the requested display is not enabled on this VM")
)

// handleTunnelStream opens (or preflights) a tunnel session for this
// (server, VM, display).
//
//   - HEAD: preflight. Validates the display and reports capacity
//     without creating a session: 200 ok, 400/503 display problems,
//     429 at the session cap.
//   - GET (h1 Upgrade) / CONNECT (h2 extended CONNECT): open the
//     session and run it to its terminal.
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

	if r.Method == http.MethodHead {
		a.tunMu.Lock()
		capped := len(a.tunnels) >= a.maxTunnels
		a.tunMu.Unlock()
		if capped {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	a.tunMu.Lock()
	if len(a.tunnels) >= a.maxTunnels {
		a.tunMu.Unlock()
		writeErr(w, http.StatusTooManyRequests,
			"too many tunnel sessions open (limit %d); try again shortly", a.maxTunnels)
		return
	}
	sid := newTunnelID()
	s := newPipeSession()
	a.tunnels[sid] = s
	a.tunMu.Unlock()
	defer a.dropTunnel(sid)

	wsSess, err := a.upgrade(w, r)
	if err != nil {
		// The upgrader already wrote the 4xx/5xx response.
		return
	}
	a.runStream(wsSess, "tunnel "+name+" "+display, func(ctx context.Context, in io.Reader, out io.Writer) error {
		err := b.Tunnel(ctx, name, target, in, out)
		if err != nil {
			log.Printf("tunnel %s %s: %v", name, target, err)
		}
		return err
	}, r.Context())
}

// handleTunnelStatus reports how many tunnel sessions are currently
// open (all of them: the registry is global). The tunnel has no
// per-(server, VM) exclusivity — several concurrent sessions per VM
// are the SPICE case — so the count is the introspection the UI and
// the e2e settle use. 200 {"open":N}; unknown server -> 404.
func (a *API) handleTunnelStatus(w http.ResponseWriter, r *http.Request) {
	if _, _, err := a.backend(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}
	a.tunMu.Lock()
	open := len(a.tunnels)
	a.tunMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]int{"open": open})
}

// dropTunnel removes a finished session from the registry.
func (a *API) dropTunnel(sid string) {
	a.tunMu.Lock()
	defer a.tunMu.Unlock()
	delete(a.tunnels, sid)
}
