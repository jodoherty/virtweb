package api

// Stream sessions — the serial console and the generic TCP tunnel are
// one WebSocket session apiece, bridged into the backend's pipes:
//
//	ws binary in  --inWrite--(pipe)--inRead--> backend stdin
//	backend stdout --outWrite--(pipe)--outRead-- ws binary out
//
// One WebSocket connection per logical session (a SPICE channel is a
// session; nothing is multiplexed above the transport). The transport
// is RFC 6455 over the same TLS listener as the rest of the API:
// browsers negotiate HTTP/2 (extended CONNECT, RFC 8441) over ALPN and
// ride one h2 connection per tab for all of their sessions; the plain
// HTTP/1.1 Upgrade path stays live as the tooling/fallback leg.
//
// Liveness:
//   - HTTP/1.1: the ws library's keepalive probes a silent peer with a
//     ping once the idle window is reached and considers the connection
//     dead after a second window. The window is the -keepalive flag.
//   - HTTP/2: the streams expose no per-stream deadline, so the
//     extended-CONNECT session runs with both windows zero (the library
//     refuses a nonzero window there, 501) and liveness is the
//     transport's job — the browser's h2 PINGs and the reverse proxy's
//     timeouts. A severed session is re-established by the client.
//
// Terminal state: the session's close code carries it. A clean end is
// 1000; a failed end is closeSessionError (1011) and the close reason
// is the server's message (bounded on the wire to 123 bytes by the ws
// package; the full message stays in the server log). A client that
// vanishes ends the session without a close — the server sees the
// transport die and kills the backend through the pipes.
//
// Conflict and limits are ordinary pre-upgrade HTTP responses (409,
// 429, 400, 503): the browser's WebSocket API does not surface the
// handshake status, so the clients preflight with a HEAD to the same
// URL (no session is created) before opening the connection.

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jodoherty/virtweb/internal/ws"
)

// closeSessionError is the application close code for a session that
// failed server-side (the ws library reserves 1011 for unexpected
// conditions; RFC 6455 §7.4 makes it the natural application code for
// "the server had to close, and here is why").
const closeSessionError = 1011

// maxWSMessage bounds a single ws message on either leg (the browser
// and the e2e harness chunk their writes below it; the bound also caps
// the reassembly allocation on a hostile client).
const maxWSMessage = 512 << 10

// h1WarnInterval bounds how often the HTTP/1.1 degradation warning is
// repeated for one client (see warnH1).
const h1WarnInterval = 5 * time.Minute

var errStreamClosed = errors.New("stream session is closed")

// pipeSession is the shared server-side session for the byte-stream
// endpoints (serial console, TCP tunnels): websocket binary messages
// are written into inWrite, and the backend's output is read from
// outRead and written back as binary messages.
type pipeSession struct {
	mu       sync.Mutex
	inWrite  *io.PipeWriter // session input: ws binary messages write here
	outRead  *io.PipeReader // session output: the downlink pump reads here
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

// write queues session input bytes. The dead check takes the mutex, but
// the pipe write itself must run WITHOUT it: inWrite.Write blocks until
// the backend reads, and the backend's own s.close (which needs the same
// mutex) is what closes inRead to unblock a write after the session is
// over. Holding the mutex across the write would deadlock — the writer
// waits on the backend to read, and the backend waits on the writer to
// release the lock. The main loop is the only writer, so no other write
// can interleave; once the lock is down, a concurrent close just makes
// the write return ErrClosed, which is correct (the session is ending).
func (s *pipeSession) write(p []byte) error {
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return errStreamClosed
	}
	w := s.inWrite
	s.mu.Unlock()
	_, err := w.Write(p)
	return err
}

// close terminates the session: closing the pipe ends makes the
// backend's read fail and the downlink pump's read hit EOF. It is the
// disconnect mechanism — after a hijacked upgrade the request context
// is NOT cancelled on client disconnect (the server no longer owns the
// connection), so killing the backend comes from here, not from a
// context.
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

// maxTerminalMessage bounds a terminal close reason. The wire cap is
// 123 bytes (close frame: 2-byte code + reason); bound it earlier so
// the ellipsis fits, and back off to a rune boundary so the reason
// stays valid UTF-8.
const maxTerminalMessage = 119

func truncateTerminalMessage(s string) string {
	if len(s) <= maxTerminalMessage {
		return s
	}
	t := s[:maxTerminalMessage]
	for len(t) > 0 && t[len(t)-1]&0xC0 == 0x80 { // UTF-8 continuation byte: cut a rune in half
		t = t[:len(t)-1]
	}
	return t + "…"
}

// upgrade switches the request to a stream session on the upgrader
// matching the protocol leg: the extended-CONNECT leg (HTTP/2) runs
// with both deadline windows zero — the library refuses a nonzero
// window on a stream that cannot enforce one (501) — while the plain
// Upgrade leg (HTTP/1.1) carries the keepalive window, where a
// hijacked net.Conn always enforces deadlines.
func (a *API) upgrade(w http.ResponseWriter, r *http.Request) (*ws.Session, error) {
	if r.Method == http.MethodConnect {
		return a.upH2.Upgrade(w, r)
	}
	s, err := a.upH1.Upgrade(w, r)
	if err == nil {
		a.warnH1(r)
	}
	return s, err
}

// warnH1 logs a verbose, throttled warning when a stream session
// established over the HTTP/1.1 Upgrade path instead of HTTP/2. h1 is
// a supported fallback (tooling, h2-less browsers), so the warning is
// not an error — but a browser session that lands here means ALPN was
// not negotiated (client configuration or a middlebox), which changes
// the session's liveness semantics and subjects it to the browser's
// per-origin connection budget, so it is worth seeing. It is throttled
// per client (mTLS certificate CN, else the peer address) to
// h1WarnInterval so a chattering client cannot flood the log;
// suppressed occurrences are counted and reported on the next
// warning.
func (a *API) warnH1(r *http.Request) {
	client := ""
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		client = r.TLS.PeerCertificates[0].Subject.CommonName
	}
	if client == "" {
		client = r.RemoteAddr
	}

	a.h1mu.Lock()
	now := time.Now()
	w, ok := a.h1warn[client]
	if ok && now.Sub(w.last) < h1WarnInterval {
		w.suppressed++
		a.h1mu.Unlock()
		return
	}
	// Prune while we hold the lock: the map is keyed by client and
	// would otherwise grow without bound.
	if len(a.h1warn) > 1024 {
		for k, v := range a.h1warn {
			if now.Sub(v.last) >= h1WarnInterval && k != client {
				delete(a.h1warn, k)
			}
		}
	}
	suppressed := 0
	if ok {
		suppressed = w.suppressed
	}
	a.h1warn[client] = &h1Warn{last: now}
	a.h1mu.Unlock()

	msg := "ws: session established over HTTP/1.1 (client=" + client + "): h2 was not negotiated — keepalive-driven liveness is on, and the session counts against the client's per-origin connection budget. If this is a browser session, check ALPN (client and any middlebox). "
	if suppressed > 0 {
		msg += strconv.Itoa(suppressed) + " further h1 sessions from this client were suppressed in the last " + h1WarnInterval.String() + ". "
	}
	log.Printf("WARN %s", msg)
}

type h1Warn struct {
	last       time.Time
	suppressed int
}

// runStream runs one upgraded session against start, the backend
// entry (virt.Backend.Console / ...Tunnel): start receives the pipe
// ends the session is bridged into and returns when the session is
// over (nil: clean end; error: the session failed, the error's text
// becomes the close reason). label names the session in the logs.
//
// Three goroutines, one owner each:
//   - the downlink pump: pipe → ws binary messages; exits when the
//     pipe is exhausted (the backend finished) or the transport dies.
//   - the uplink reader: the session's read loop; delivers binary
//     messages to the pipe and owns the terminal handling — on a
//     backend end it sends the close frame (Shutdown) and reads to the
//     terminal (Drain); on a client end it kills the pipes.
//   - the backend: start itself; on return it closes the pipes (which
//     is what both pumps observe as "the session is over").
//
// runStream returns only once every one has exited, so the response
// (the h1 connection or the h2 stream) finishes after the transport is
// fully down.
func (a *API) runStream(sess *ws.Session, label string, start func(ctx context.Context, in io.Reader, out io.Writer) error, ctx context.Context) {
	s := newPipeSession()

	// Backend: run the session, then close the pipes. The request
	// context still matters here for server shutdown (Shutdown cancels
	// it and the backend kills the remote session); client
	// disconnects are NOT signalled through it (post-hijack the server
	// does not cancel it) — those come through the pipes.
	backendDone := make(chan error, 1)
	go func() {
		err := start(ctx, s.inRead, s.outWrite)
		backendDone <- err
		s.close()
	}()

	// Downlink pump: pipe → ws. A write failure means the transport
	// died; the bytes were lost with it, so stop.
	downlinkDone := make(chan struct{})
	go func() {
		defer close(downlinkDone)
		buf := make([]byte, 32<<10)
		for {
			n, err := s.outRead.Read(buf)
			if n > 0 {
				if werr := sess.WriteBinary(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return // EOF (backend finished) or pipe closed
			}
		}
	}()

	// Uplink reader: the session's read loop (single owner). Binary
	// messages go to the pipe; text is not part of this transport.
	type upMsg struct {
		data []byte
		err  error
	}
	upCh := make(chan upMsg, 8)
	readDone := make(chan struct{})
	readStop := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			op, data, err := sess.ReadMessage()
			m := upMsg{err: err}
			if err == nil && op == ws.OpBinary {
				m.data = data
			}
			select {
			case upCh <- m:
			case <-readStop:
				return
			}
			if err != nil {
				return // clean EOF (1000) or *ws.CloseError
			}
		}
	}()

	stopRead := func() {
		close(readStop)
		// Unblock a ReadMessage that is parked in the ws layer: Close
		// tears the transport down, never blocks, and is safe from any
		// goroutine. The reader then observes the terminal error.
		sess.Close()
	}

	for {
		select {
		case err := <-backendDone:
			// Backend finished: the downlink pump flushes everything
			// the backend wrote, then we close politely with the
			// terminal code — 1000 for a clean end, closeSessionError
			// with the server's message otherwise — and let the reader
			// run the transport to its terminal.
			<-downlinkDone
			if err == nil {
				_ = sess.Shutdown(1000, "session ended")
			} else {
				_ = sess.Shutdown(closeSessionError, truncateTerminalMessage(err.Error()))
				// A backend that fails before the client has finished the
				// handshake (an immediate attach failure) leaves its close frame
				// queued on the stream: tearing the stream down right now resets
				// it before the frame reaches the wire, and the client sees 1006
				// instead of 1011, losing the reason. Give the client a moment
				// to complete the handshake and read the close frame. For a
				// session that has been running, the handshake is long done and
				// this cost is negligible; it matters only on instant failure.
				time.Sleep(50 * time.Millisecond)
			}
			stopRead()
			<-readDone
			s.close()
			if err != nil {
				log.Printf("stream session %s: %v", label, err)
			}
			return
		case m := <-upCh:
			if m.err != nil {
				// Client gone (clean close, abnormal end, or keepalive
				// timeout): kill the session; the backend ends when the
				// pipes close, and there is no terminal to report —
				// the client is not there to receive one.
				s.close()
				stopRead()
				<-readDone
				<-downlinkDone
				return
			}
			if m.data != nil {
				_ = s.write(m.data) // session-closed is fine: the backend is ending
			}
		}
	}
}
