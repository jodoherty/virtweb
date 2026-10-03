package api

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- test-side frame helpers (mirror the client FrameDecoder) ----

type gotFrame struct {
	tag     byte
	payload []byte
}

// framesFrom parses whole frames from a byte buffer and returns them
// plus the trailing incomplete bytes.
func framesFrom(buf []byte) ([]gotFrame, []byte, error) {
	var frames []gotFrame
	off := 0
	for off+5 <= len(buf) {
		tag := buf[off]
		n := binary.BigEndian.Uint32(buf[off+1 : off+5])
		if int64(n) > maxFramePayload {
			return nil, nil, errFrameTooLarge
		}
		end := off + 5 + int(n)
		if end > len(buf) {
			break
		}
		frames = append(frames, gotFrame{tag, buf[off+5 : end]})
		off = end
	}
	return frames, buf[off:], nil
}

// encodeTestFrame builds one frame for test request bodies.
func encodeTestFrame(t *testing.T, tag byte, payload []byte) []byte {
	t.Helper()
	out := make([]byte, 5+len(payload))
	out[0] = tag
	binary.BigEndian.PutUint32(out[1:], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

// testSink implements http.ResponseWriter + http.Flusher, recording
// into a synchronized buffer so the pump's goroutine can write while
// the test polls the body (ResponseRecorder.Body is not
// concurrency-safe).
type testSink struct {
	mu   sync.Mutex
	body bytes.Buffer
}

func (s *testSink) Header() http.Header { return http.Header{} }

func (s *testSink) WriteHeader(int) {}

func (s *testSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.Write(p)
}

func (s *testSink) Flush() {}

func (s *testSink) bodyBytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.Bytes()
}

// waitForFrames polls a body snapshot until cond passes or the
// deadline elapses.
func waitForFrames(t *testing.T, snap func() []byte, timeout time.Duration, cond func([]gotFrame) bool) []gotFrame {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		buf := snap()
		frames, rest, err := framesFrom(buf)
		if err != nil {
			t.Fatalf("bad frames: %v (body %q)", err, buf)
		}
		if cond(frames) {
			if len(rest) > 0 && len(frames) > 0 && frames[len(frames)-1].tag == frameKeepalive {
				// a half-written frame may still be arriving; ignore
			}
			return frames
		}
		time.Sleep(5 * time.Millisecond)
	}
	frames, _, _ := framesFrom(snap())
	return frames
}

func TestWriteHello(t *testing.T) {
	var buf bytes.Buffer
	if err := writeHello(&buf, nil); err != nil {
		t.Fatal(err)
	}
	want := []byte{frameHello, 0, 0, 0, 2, frameVersion, helloFlags}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("hello = %v, want %v", buf.Bytes(), want)
	}
}

func TestWriteFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	for _, payload := range [][]byte{nil, []byte("x"), []byte(strings.Repeat("y", 1000))} {
		if err := writeFrame(&buf, nil, frameData, payload); err != nil {
			t.Fatal(err)
		}
	}
	frames, rest, err := framesFrom(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 0 {
		t.Fatalf("leftover bytes: %d", len(rest))
	}
	if len(frames) != 3 || frames[0].tag != frameData ||
		len(frames[0].payload) != 0 || string(frames[1].payload) != "x" ||
		len(frames[2].payload) != 1000 {
		t.Fatalf("frames = %+v", frames)
	}
}

func TestReadFrameBody(t *testing.T) {
	cases := []struct {
		name    string
		body    []byte
		wantTag byte
		wantPL  string
		wantErr bool
	}{
		{"data", encodeTestFrame(t, frameData, []byte("hello")), frameData, "hello", false},
		{"keepalive", encodeTestFrame(t, frameKeepalive, nil), frameKeepalive, "", false},
		{"short header", []byte{frameData, 0, 0, 0}, 0, "", true},
		{"truncated payload", encodeTestFrame(t, frameData, []byte("abcd"))[:6], 0, "", true},
		{"oversized", append([]byte{frameData}, 0, 0, 0xff, 0xff, 0xff)[:5], 0, "", true},
	}
	for _, tc := range cases {
		req := httptest.NewRequest("POST", "/", bytes.NewReader(tc.body))
		tag, payload, err := readFrameBody(req, maxInputBody)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: want error, got tag=%#x payload=%q", tc.name, tag, payload)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if tag != tc.wantTag || string(payload) != tc.wantPL {
			t.Errorf("%s: tag=%#x payload=%q, want %#x %q", tc.name, tag, payload, tc.wantTag, tc.wantPL)
		}
	}
}

// TestPumpFramed: DATA frames carry the stream bytes, KEEPALIVE frames
// appear while idle and stop during traffic, and done closes when the
// source is exhausted.
func TestPumpFramed(t *testing.T) {
	pr, pw := io.Pipe()
	w := &testSink{}
	done := make(chan struct{})
	go pumpFramed(pr, w, 40*time.Millisecond, nil, done)

	if _, err := pw.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	frames := waitForFrames(t, w.bodyBytes, 2*time.Second, func(f []gotFrame) bool {
		return len(f) >= 1 && f[0].tag == frameData && string(f[0].payload) == "abc"
	})
	if len(frames) < 1 || frames[0].tag != frameData || string(frames[0].payload) != "abc" {
		t.Fatalf("first frames = %+v, want DATA abc", frames)
	}

	// Idle long enough -> a KEEPALIVE arrives.
	frames = waitForFrames(t, w.bodyBytes, 2*time.Second, func(f []gotFrame) bool {
		for _, fr := range f {
			if fr.tag == frameKeepalive {
				return true
			}
		}
		return false
	})
	found := false
	for _, fr := range frames {
		if fr.tag == frameKeepalive {
			found = true
			if len(fr.payload) != 0 {
				t.Fatalf("keepalive payload = %v, want empty", fr.payload)
			}
		}
	}
	if !found {
		t.Fatalf("no keepalive after idle, frames = %+v", frames)
	}

	// Traffic again -> more DATA, pump ends on EOF.
	if _, err := pw.Write([]byte("def")); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not finish after EOF")
	}
	frames, rest, err := framesFrom(w.bodyBytes())
	if err != nil {
		t.Fatal(err)
	}
	last := frames[len(frames)-1]
	if last.tag != frameData || string(last.payload) != "def" || len(rest) != 0 {
		t.Fatalf("final frames = %+v rest=%d, want ending DATA def", frames, len(rest))
	}
}

// TestPumpFramedNoKeepalive: keepalive disabled -> DATA frames only.
func TestPumpFramedNoKeepalive(t *testing.T) {
	pr, pw := io.Pipe()
	w := &testSink{}
	done := make(chan struct{})
	go pumpFramed(pr, w, 0, nil, done)
	if _, err := pw.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not finish")
	}
	time.Sleep(20 * time.Millisecond) // any (wrong) keepalive would have gone by now
	frames, _, err := framesFrom(w.bodyBytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || frames[0].tag != frameData {
		t.Fatalf("frames = %+v, want one DATA", frames)
	}
}

// TestPumpFramedTerminal: the terminal event is emitted as the LAST
// frame, in both arrival orders (event before pipe EOF — the handler's
// send-then-close order — and EOF first with the event already
// queued).
func TestPumpFramedTerminal(t *testing.T) {
	cases := []struct {
		name    string
		event   terminalEvent
		wantTag byte
		wantMsg string
	}{
		{"clean end", terminalEvent{tag: frameEnd}, frameEnd, ""},
		{"error end", terminalEvent{tag: frameError, message: "domain 'vm1' is not running"}, frameError, "domain 'vm1' is not running"},
	}
	for _, tc := range cases {
		t.Run(tc.name+" (event before EOF)", func(t *testing.T) {
			sink := &testSink{}
			pr, pw := io.Pipe()
			termCh := make(chan terminalEvent, 1)
			done := make(chan struct{})
			go pumpFramed(pr, sink, 0, termCh, done)
			pw.Write([]byte("out"))
			frames := waitForFrames(t, sink.bodyBytes, 2*time.Second, func(f []gotFrame) bool {
				return len(f) >= 1 && f[0].tag == frameData
			})
			if len(frames) < 1 || frames[0].tag != frameData || string(frames[0].payload) != "out" {
				t.Fatalf("first frames = %+v, want DATA out", frames)
			}
			// The handler's order: send the event, then close the pipe.
			termCh <- tc.event
			pw.Close()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("pump did not finish")
			}
			all, rest, err := framesFrom(sink.bodyBytes())
			if err != nil || len(rest) != 0 {
				t.Fatalf("final frames = %+v rest=%d err=%v", all, len(rest), err)
			}
			if len(all) < 2 {
				t.Fatalf("frames = %+v, want DATA + terminal", all)
			}
			last := all[len(all)-1]
			if last.tag != tc.wantTag || string(last.payload) != tc.wantMsg {
				t.Fatalf("terminal frame = %+v, want tag %#x msg %q", last, tc.wantTag, tc.wantMsg)
			}
			// No frame may precede or follow: exactly DATA + terminal.
			if got := len(all); got != 2 {
				t.Fatalf("frame count = %d, want exactly 2 (DATA + terminal): %+v", got, all)
			}
		})

		t.Run(tc.name+" (EOF first, event queued)", func(t *testing.T) {
			sink := &testSink{}
			pr, pw := io.Pipe()
			termCh := make(chan terminalEvent, 1)
			done := make(chan struct{})
			go pumpFramed(pr, sink, 0, termCh, done)
			// Close the pipe first; the event is already queued (the
			// handler sends it just before closing, so at EOF it must
			// be there).
			termCh <- tc.event
			pw.Close()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("pump did not finish")
			}
			all, rest, err := framesFrom(sink.bodyBytes())
			if err != nil || len(rest) != 0 {
				t.Fatalf("frames = %+v rest=%d err=%v", all, len(rest), err)
			}
			if len(all) != 1 || all[0].tag != tc.wantTag || string(all[0].payload) != tc.wantMsg {
				t.Fatalf("frames = %+v, want single terminal %q", all, tc.wantMsg)
			}
		})
	}
}

// TestPumpFramedNoTerminal: no terminal event (client went away) ->
// the stream ends silently on EOF.
func TestPumpFramedNoTerminal(t *testing.T) {
	sink := &testSink{}
	pr, pw := io.Pipe()
	termCh := make(chan terminalEvent, 1) // never written to
	done := make(chan struct{})
	go pumpFramed(pr, sink, 0, termCh, done)
	pw.Write([]byte("x"))
	pw.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not finish")
	}
	all, _, _ := framesFrom(sink.bodyBytes())
	if len(all) != 1 || all[0].tag != frameData {
		t.Fatalf("frames = %+v, want one DATA only", all)
	}
}

// TestStreamEndpointsSpeakFramedProtocol: a live stream opens with a
// HELLO frame; console input must be a frame (raw bytes -> 400).
func TestStreamEndpointsSpeakFramedProtocol(t *testing.T) {
	ts, _, _ := newTestAPI(t)
	id := addTestServer(t, ts)
	client := &http.Client{}

	res, err := client.Get(ts.URL + "/api/servers/" + id + "/vms/vm1/console/stream")
	if err != nil {
		t.Fatal(err)
	}
	// Read just the HELLO frame (5-byte header + 2-byte payload);
	// keep the stream open.
	hdr := make([]byte, 7)
	if _, err := io.ReadFull(res.Body, hdr); err != nil {
		t.Fatal(err)
	}
	want := []byte{frameHello, 0, 0, 0, 2, frameVersion, helloFlags}
	if !bytes.Equal(hdr, want) {
		t.Fatalf("first stream bytes = %v, want HELLO %v", hdr, want)
	}
	defer res.Body.Close()

	// Raw (unframed) input is refused while the session is open.
	inRes, err := client.Post(ts.URL+"/api/servers/"+id+"/vms/vm1/console/input",
		"application/octet-stream", strings.NewReader("raw"))
	if err != nil {
		t.Fatal(err)
	}
	inRes.Body.Close()
	if inRes.StatusCode != http.StatusBadRequest {
		t.Fatalf("raw input status = %d, want 400", inRes.StatusCode)
	}

	// A framed DATA frame is accepted.
	inRes, err = client.Post(ts.URL+"/api/servers/"+id+"/vms/vm1/console/input",
		"application/octet-stream",
		bytes.NewReader(encodeTestFrame(t, frameData, []byte("ping"))))
	if err != nil {
		t.Fatal(err)
	}
	inRes.Body.Close()
	if inRes.StatusCode != http.StatusNoContent {
		t.Fatalf("framed input status = %d, want 204", inRes.StatusCode)
	}
}
