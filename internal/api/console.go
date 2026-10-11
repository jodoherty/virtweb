package api

import (
	"context"
	"io"
	"net/http"
)

// The serial console is one WebSocket session bridged into
// virt.Backend.Console (see stream.go for the transport): browser
// keystrokes arrive as binary messages, console output leaves as
// binary messages, and the session's terminal state is the close
// code/reason (1000 clean, 1011 + the server's message on failure).
//
// There is one session per (server, VM); a second concurrent session
// gets 409 (pre-upgrade, so it is an ordinary HTTP response — the
// browser preflights with HEAD before opening the connection). The
// client is tracked so the "console in use" indicator survives.

// handleConsoleStream opens (or preflights) the console session for
// this (server, VM).
//
//   - HEAD: preflight. Reports availability without creating a
//     session: 200 open, 409 in use, 429 at the session cap. The
//     browser uses it to surface the conflict before its WebSocket
//     handshake (which cannot report the handshake status).
//   - GET (h1 Upgrade) / CONNECT (h2 extended CONNECT): open the
//     session and run it to its terminal.
func (a *API) handleConsoleStream(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("name")
	_, b, err := a.backend(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}
	key := id + "/" + name

	if r.Method == http.MethodHead {
		a.conMu.Lock()
		_, busy := a.consoles[key]
		capped := len(a.consoles) >= a.maxConsoles
		a.conMu.Unlock()
		switch {
		case busy:
			w.WriteHeader(http.StatusConflict)
		case capped:
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusOK)
		}
		return
	}

	a.conMu.Lock()
	if _, ok := a.consoles[key]; ok {
		a.conMu.Unlock()
		writeErr(w, http.StatusConflict, "console for %q is already open", name)
		return
	}
	if len(a.consoles) >= a.maxConsoles {
		a.conMu.Unlock()
		writeErr(w, http.StatusTooManyRequests,
			"too many console sessions open (limit %d); try again shortly", a.maxConsoles)
		return
	}
	s := newPipeSession()
	a.consoles[key] = s
	a.conMu.Unlock()
	defer a.dropConsole(key, s)

	wsSess, err := a.upgrade(w, r)
	if err != nil {
		// The upgrader already wrote the 4xx/5xx response.
		return
	}
	a.runStream(wsSess, "console "+name, func(ctx context.Context, in io.Reader, out io.Writer) error {
		return b.Console(ctx, name, in, out)
	}, r.Context())
}

// handleConsoleStatus reports whether a console session is currently
// open for this (server, VM) — the "console in use" indicator. One
// session per (server, VM): open means a session exists in another
// tab or window. 200 {"open":true|false}; unknown server -> 404.
func (a *API) handleConsoleStatus(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("name")
	if _, _, err := a.backend(id); err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}
	a.conMu.Lock()
	s := a.consoles[id+"/"+name]
	open := s != nil
	a.conMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"open": open})
}

// dropConsole removes a finished session from the registry so the key
// can be reused by a new session.
func (a *API) dropConsole(key string, s *pipeSession) {
	a.conMu.Lock()
	defer a.conMu.Unlock()
	if cur, ok := a.consoles[key]; ok && cur == s {
		delete(a.consoles, key)
	}
}
