package api

import (
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"time"
)

// Transport wire protocol — length-framed, tagged frames carried on
// the plain-HTTP streams (console output/input, tunnel stream/input).
//
//   frame := [tag:1][len:4 big-endian][payload]
//
// The framing exists for two reasons:
//
//  1. Idle keepalives. A TCP reverse proxy with its own read/idle
//     timeouts would otherwise cut quiet streams (an idle serial
//     console or a static display is byte-silent). Each stream emits
//     KEEPALIVE frames while idle, so no stream can go quiet for
//     longer than ~2x the keepalive interval regardless of proxy
//     topology (L4/h2, L7, h1.1). virtweb is an opaque byte tunnel, so
//     this keepalive cannot be in-band in the guest protocol — it
//     lives one layer up, in frames the tunnel itself emits.
//
//  2. Detectable corruption. Without framing, a byte dropped mid-
//     stream desyncs a VNC/SPICE session silently; a torn frame fails
//     the channel cleanly instead.
//
// Protocol v2 (announced by the HELLO flags byte, bit
// helloTerminalFrames) adds in-band terminal frames: the session's final state
// arrives as a frame (SESSION_END, clean, or SESSION_ERROR with the
// server's message) before the stream ends, so the client reports
// WHY a session died instead of guessing from how the stream did. The
// frame shape and version are unchanged — v2 is a capability, not a
// new version. (Future v3 candidates: in-band client-side terminal
// frames, negotiated the same way.)
//
// The browser side parses the same frames in web/static/duplex.js
// (FrameDecoder); consumers (xterm, noVNC, the SPICE fork) only ever
// see DATA payloads. The e2e harness mirrors the parser in
// e2e/src/frames.js.

const (
	frameHello     byte = 0x01
	frameData      byte = 0x02
	frameKeepalive byte = 0x03

	// Wire protocol v2 (announced by the HELLO flags byte, see
	// helloTerminalFrames): in-band terminal frames. The session's
	// final state arrives as a frame before the stream ends, so the
	// client can distinguish "ended cleanly" from "failed" and see the
	// server's reason instead of guessing from how the stream died.
	// Both are downlink-only: the client's terminal state is simply
	// its stream abort, which the server already sees.
	frameEnd   byte = 0x04 // clean end; payload: optional note
	frameError byte = 0x05 // failed end; payload: error message
)

// The frame version stays at 1: the frame shape (tag + len + payload)
// is unchanged — v2 is carried by the HELLO flags capability bits,
// not by bumping the version. The version would increment for a
// change to the frame shape or the meaning of HELLO/DATA.
const frameVersion byte = 1

// helloTerminalFrames is the HELLO flags bit announcing that the
// downlink may end with a frameEnd/frameError terminal frame.
const helloTerminalFrames byte = 0x01

// helloFlags is the server's current capability word.
const helloFlags = helloTerminalFrames

// maxTerminalMessage bounds the terminal frame's message payload
// (ssh/virsh errors can be long; the UI shows it verbatim).
const maxTerminalMessage = 512

// terminalEvent is the session's terminal state, delivered to the
// pump so it can emit the terminal frame as the LAST frame of the
// stream (any pipe data the backend wrote before the end is flushed
// as DATA first, since the session is over).
type terminalEvent struct {
	tag     byte // frameEnd or frameError
	message string
}

// truncateTerminalMessage bounds a terminal frame's message.
func truncateTerminalMessage(s string) string {
	if len(s) <= maxTerminalMessage {
		return s
	}
	return s[:maxTerminalMessage] + "…"
}

// maxFramePayload bounds one frame's payload in the downlink
// parser (16 MiB is far beyond any real chunk; it bounds the
// reassembly allocation, it is not a performance knob). Uplink
// payloads are bounded per endpoint (maxInputBody /
// maxTunnelInputBody).
const maxFramePayload = 16 << 20

// defaultKeepalive is how long a stream may sit idle before a
// KEEPALIVE frame goes out (the -keepalive flag sets it per server;
// 0 disables keepalives entirely).
const defaultKeepalive = 15 * time.Second

// frameTooLarge is reported for frames whose declared length exceeds
// the accepted bound.
var errFrameTooLarge = errors.New("frame payload exceeds the maximum")

// writeFrame writes one frame to w and flushes it (each frame is a
// flush unit, so proxy pass-through stays live).
func writeFrame(w io.Writer, f http.Flusher, tag byte, payload []byte) error {
	hdr := [5]byte{tag}
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

// writeHello sends the HELLO frame: [version][flags]. It is the first
// frame of every downlink stream.
func writeHello(w io.Writer, f http.Flusher) error {
	return writeFrame(w, f, frameHello, []byte{frameVersion, helloFlags})
}

// pumpFramed frames the bytes of r as DATA frames into w (flushing
// each), emitting KEEPALIVE frames while the stream has been idle for
// keepalive (keepalive <= 0 disables keepalives). The session's
// terminal state arrives on termCh (buffered, may be nil): the pump
// flushes any pending pipe output as DATA, emits the terminal frame
// as the LAST frame, and ends the stream. It returns when r is exhausted
// (delivering a queued terminal event as the last frame, if any),
// when the terminal event is delivered, or when a write fails; done
// is closed on return, only once all writes have completed (the
// caller may then finish the response).
func pumpFramed(r io.Reader, w http.ResponseWriter, keepalive time.Duration, termCh <-chan terminalEvent, done chan struct{}) {
	defer close(done)
	flusher, _ := w.(http.Flusher)

	// A reader goroutine feeds chunks to the pump loop over a
	// buffered channel; it copies each chunk so the pump owns the
	// bytes it writes. stop ends the reader when the pump exits (a
	// write failure) — without it the reader would block on its send
	// forever.
	type result struct {
		data []byte
		err  error
	}
	rc := make(chan result, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			res := result{err: err}
			if n > 0 {
				res.data = make([]byte, n)
				copy(res.data, buf[:n])
			}
			select {
			case rc <- res:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var tickC <-chan time.Time
	if keepalive > 0 {
		// Tick at half the interval so an idle stream emits a
		// keepalive within [keepalive, 1.5*keepalive) of going
		// quiet, not up to 2*keepalive later.
		tick := time.NewTicker(keepalive / 2)
		defer tick.Stop()
		tickC = tick.C
	}
	last := time.Now()
	for {
		select {
		case res := <-rc:
			if res.data != nil {
				if err := writeFrame(w, flusher, frameData, res.data); err != nil {
					return
				}
				last = time.Now()
			}
			if res.err != nil {
				// Stream exhausted. If the terminal event is already
				// queued (the handler sends it just before closing the
				// pipe), emit it as the last frame; otherwise exit
				// silently (client went away: no terminal state to
				// report).
				select {
				case ev := <-termCh:
					if err := writeFrame(w, flusher, ev.tag, []byte(ev.message)); err != nil {
						return
					}
				default:
				}
				return
			}
		case <-tickC:
			if time.Since(last) >= keepalive {
				if err := writeFrame(w, flusher, frameKeepalive, nil); err != nil {
					return
				}
				last = time.Now()
			}
		case ev := <-termCh:
			// Session over: flush whatever output is still in flight
			// as DATA (the client is entitled to what the backend
			// wrote before the end), then emit the terminal frame as
			// the LAST frame and end the stream.
			for {
				select {
				case res := <-rc:
					if res.data != nil {
						if err := writeFrame(w, flusher, frameData, res.data); err != nil {
							return
						}
					}
					if res.err != nil {
						if err := writeFrame(w, flusher, ev.tag, []byte(ev.message)); err != nil {
							return
						}
						return
					}
				}
			}
		}
	}
}

// readFrameBody parses exactly one frame from the request body:
// [tag][len:4 BE][payload], with the payload bounded by max. It
// reports the tag and payload. A short/absent body or an over-large
// length is an error (the caller returns 400).
func readFrameBody(r *http.Request, max int) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(io.LimitReader(r.Body, int64(len(hdr))+int64(max)), hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > uint32(max) {
		return 0, nil, errFrameTooLarge
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r.Body, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}
