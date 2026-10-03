package api

import (
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
)

// The serial console is bridged over plain HTTP — no WebSocket. The
// browser consumes the console output as a ReadableStream over a
// chunked, flushed GET response, and sends keystrokes as small POSTs.
// Both requests belong to one consoleSession, which owns the pipes
// wired into virt.Backend.Console:
//
//	browser POST body --inWrite--(pipe)--inRead--> Backend stdin
//	Backend stdout --outWrite--(pipe)--outRead--> flushed GET response
//
// There is one session per (server, VM); a second concurrent stream
// gets 409. Because no connection is hijacked, r.Context() is
// cancelled on client disconnect and is passed straight to the
// backend, which kills the remote ssh session.

// maxInputBody caps a single input POST (keystroke bursts are tiny).
const maxInputBody = 64 << 10

var errStreamClosed = errors.New("stream session is closed")

// pipeSession is the shared server-side session for byte-stream
// endpoints (serial console, TCP tunnels): browser POST bodies are
// written into inWrite, and the backend's output is read from outRead
// and pumped to the flushed GET response.
type pipeSession struct {
	mu       sync.Mutex
	inWrite  *io.PipeWriter // console input: input POSTs write here
	outRead  *io.PipeReader // console output: the stream handler reads here
	inRead   *io.PipeReader // handed to the backend
	outWrite *io.PipeWriter // handed to the backend
	dead     bool
}

func newPipeSession() *pipeSession {
	inRead, inWrite := io.Pipe()
	outRead, outWrite := io.Pipe()
	return &pipeSession{
		inWrite:  inWrite,
		outRead:  outRead,
		inRead:   inRead,
		outWrite: outWrite,
	}
}

// write queues stream input bytes.
func (s *pipeSession) write(p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return errStreamClosed
	}
	_, err := s.inWrite.Write(p)
	return err
}

// close terminates the session: closing the pipe ends makes the
// backend's read fail and the stream handler's read hit EOF.
func (s *pipeSession) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return
	}
	s.dead = true
	_ = s.outWrite.Close()
	_ = s.inRead.Close()
}

// handleConsoleStream serves the console output: a chunked, flushed
// text response that the browser iterates as a ReadableStream.
func (a *API) handleConsoleStream(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("name")
	_, b, err := a.backend(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}
	key := id + "/" + name
	a.conMu.Lock()
	if s, ok := a.consoles[key]; ok && !s.dead {
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

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	// Reverse proxies must pass the stream through unbuffered and
	// uncompressed (h2 or not); see AGENTS.md.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}

	// Run the console session; when it ends (VM hung up, session error,
	// or the client disconnected and r.Context() cancelled it), close the
	// pipes so the pump below terminates.
	go func() {
		err := b.Console(r.Context(), name, s.inRead, s.outWrite)
		s.close()
		if err != nil {
			log.Printf("console %s: %v", name, err)
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
	case <-pumpDone:          // console output ended
	}
	s.close()
	// The pump goroutine may still be inside Write/Flush; the response
	// writer must not be used concurrently with the server finishing
	// the response, so wait for it to fully drain before returning.
	<-pumpDone
}

// handleConsoleInput writes one keystroke batch to an open console
// session. The body is raw bytes (application/octet-stream).
func (a *API) handleConsoleInput(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("name")
	a.conMu.Lock()
	s := a.consoles[id+"/"+name]
	a.conMu.Unlock()
	if s == nil {
		writeErr(w, http.StatusNotFound, "no console session open for %q", name)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxInputBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read input: %v", err)
		return
	}
	if err := s.write(body); err != nil {
		writeErr(w, http.StatusNotFound, "console session for %q is not open", name)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// dropConsole removes a finished session from the registry so the key
// can be reused by a new stream.
func (a *API) dropConsole(key string, s *pipeSession) {
	a.conMu.Lock()
	defer a.conMu.Unlock()
	if cur, ok := a.consoles[key]; ok && cur == s {
		delete(a.consoles, key)
	}
}
