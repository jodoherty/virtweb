// Package proxyproto implements the PROXY protocol (v1 text and v2
// binary) for TCP listeners that sit behind a load balancer (e.g. a
// Traefik L4 pass-through router on an internal bridge network) which
// prepends the original client address to every connection.
//
// The header is read once per accepted connection, OUTSIDE TLS: the
// pipeline is  TCP accept -> PROXY header -> TLS (mTLS) -> HTTP.
package proxyproto

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"time"
)

// headerTimeout bounds the read of the PROXY header on a freshly
// accepted connection.
const headerTimeout = 5 * time.Second

// maxV1Line bounds a v1 header line (spec lines are short; HAProxy
// uses <= 128 bytes).
const maxV1Line = 256

// errNoHeader is returned when the connection does not start with a
// PROXY header (e.g. someone connected directly, bypassing the proxy).
var errNoHeader = errors.New("proxyproto: missing PROXY header")

// ppv2Sig is the 12-byte PROXY protocol v2 signature ("\r\n\r\n\x00\r\nQUIT\n").
var ppv2Sig = []byte{0x0d, 0x0a, 0x0d, 0x0a, 0x00, 0x0d, 0x0a, 0x51, 0x55, 0x49, 0x54, 0x0a}

// Parse reads a PROXY protocol header (v1 or v2) from r and returns
// the original client address, or nil when the header is
// UNKNOWN/LOCAL. It is an error if the data does not start with a
// PROXY header.
func Parse(r *bufio.Reader) (net.Addr, error) {
	head := make([]byte, 12)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, fmt.Errorf("proxyproto: short header: %w", err)
	}
	if bytes.Equal(head, ppv2Sig) {
		return parseV2(r)
	}
	if !bytes.HasPrefix(head, []byte("PROXY ")) {
		return nil, errNoHeader
	}
	return parseV1(r, head)
}

// parseV2 consumes the 4-byte header + address block following the
// v2 signature (already consumed).
func parseV2(r *bufio.Reader) (net.Addr, error) {
	// Version(4b)|Command(4b), Family(8b), Transport(8b), Length(16b).
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, fmt.Errorf("proxyproto: short v2 header: %w", err)
	}
	cmd, fam, typ := hdr[0]&0x0f, hdr[1], hdr[2]
	length := int(hdr[3])<<8 | int(hdr[4])
	if cmd != 0 && cmd != 1 { // LOCAL or PROXY
		return nil, fmt.Errorf("proxyproto: unknown v2 command %d", cmd)
	}
	if typ != 1 && typ != 2 { // TCP or UDP
		return nil, fmt.Errorf("proxyproto: unknown v2 transport %d", typ)
	}
	switch fam {
	case 0: // UNSPEC
		if length != 0 {
			return nil, fmt.Errorf("proxyproto: v2 UNSPEC with length %d", length)
		}
		return nil, nil
	case 1: // INET
		if length != 12 {
			return nil, fmt.Errorf("proxyproto: v2 INET with length %d", length)
		}
		var b [12]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, fmt.Errorf("proxyproto: short v2 address: %w", err)
		}
		return &net.TCPAddr{IP: b[0:4], Port: int(b[8])<<8 | int(b[9])}, nil
	case 2: // INET6
		if length != 36 {
			return nil, fmt.Errorf("proxyproto: v2 INET6 with length %d", length)
		}
		var b [36]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, fmt.Errorf("proxyproto: short v2 address: %w", err)
		}
		return &net.TCPAddr{IP: b[0:16], Port: int(b[32])<<8 | int(b[33])}, nil
	default:
		return nil, fmt.Errorf("proxyproto: unknown v2 family %d", fam)
	}
}

// parseV1 consumes the rest of the v1 header line (the first 12 bytes
// are in head) and parses "PROXY TCP4|TCP6 src dst sport dport" (or
// "PROXY UNKNOWN").
func parseV1(r *bufio.Reader, head []byte) (net.Addr, error) {
	buf := make([]byte, 0, 32)
	buf = append(buf, head...)
	for {
		if len(buf) >= maxV1Line {
			return nil, fmt.Errorf("proxyproto: v1 line too long")
		}
		b, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("proxyproto: truncated v1 line: %w", err)
		}
		buf = append(buf, b)
		if b == '\n' {
			break
		}
	}
	line := strings.TrimRight(string(buf), "\r\n")
	if line == "PROXY UNKNOWN" {
		return nil, nil
	}
	f := strings.Fields(line)
	if len(f) != 6 {
		return nil, fmt.Errorf("proxyproto: malformed v1 line %q", line)
	}
	if f[1] != "TCP4" && f[1] != "TCP6" {
		return nil, fmt.Errorf("proxyproto: v1 transport %q (want TCP4/TCP6)", f[1])
	}
	src := net.ParseIP(f[2])
	dst := net.ParseIP(f[3])
	sp, err1 := strconv.Atoi(f[4])
	dp, err2 := strconv.Atoi(f[5])
	if src == nil || dst == nil || err1 != nil || err2 != nil ||
		sp < 0 || sp > 65535 || dp < 0 || dp > 65535 {
		return nil, fmt.Errorf("proxyproto: malformed v1 address %q", line)
	}
	_ = dst
	return &net.TCPAddr{IP: src, Port: sp}, nil
}

// Listener is a net.Listener that validates and strips a PROXY
// protocol header from each accepted connection. When From is
// non-empty, connections whose TCP source is not in From are rejected
// before any data is read — this is what makes -proxy-protocol safe:
// only the proxy can speak PROXY to virtweb.
type Listener struct {
	inner net.Listener
	from  []*net.IPNet
}

// New wraps inner with PROXY protocol handling. from is a list of
// source IPs or CIDRs allowed to present the header (empty = no
// source restriction; virtweb's main() requires it to be non-empty).
func New(inner net.Listener, from []string) (*Listener, error) {
	l := &Listener{inner: inner}
	for _, c := range from {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !strings.Contains(c, "/") {
			if ip := net.ParseIP(c); ip != nil && ip.To4() != nil {
				c += "/32"
			} else {
				c += "/128"
			}
		}
		_, ipnet, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("proxyproto: bad source spec %q: %w", c, err)
		}
		l.from = append(l.from, ipnet)
	}
	return l, nil
}

// Accept implements net.Listener.
func (l *Listener) Accept() (net.Conn, error) {
	c, err := l.inner.Accept()
	if err != nil {
		return nil, err
	}
	if !l.allowed(c.RemoteAddr()) {
		log.Printf("proxyproto: rejecting connection from %s (not an allowed proxy source)", c.RemoteAddr())
		c.Close()
		return nil, errNoHeader
	}
	if err := c.SetReadDeadline(time.Now().Add(headerTimeout)); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReaderSize(c, 512)
	orig, err := Parse(br)
	if err := c.SetReadDeadline(time.Time{}); err != nil { // clear for TLS/HTTP
		log.Printf("proxyproto: clearing read deadline: %v", err)
	}
	if err != nil {
		log.Printf("proxyproto: %v from %s; closing", err, c.RemoteAddr())
		c.Close()
		return nil, err
	}
	// The bufio reader may hold header-adjacent bytes (e.g. the start of
	// the TLS handshake); route them through so the rest of the stack
	// sees the stream intact.
	var conn net.Conn = &restConn{Conn: c, rest: br}
	if orig != nil {
		// Report the original client as RemoteAddr (trust: only an
		// allowed proxy source can set it).
		conn = &origConn{conn, orig}
	}
	return conn, nil
}

func (l *Listener) Addr() net.Addr { return l.inner.Addr() }
func (l *Listener) Close() error   { return l.inner.Close() }

func (l *Listener) allowed(addr net.Addr) bool {
	if len(l.from) == 0 {
		return true
	}
	ip, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return false
	}
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	for _, n := range l.from {
		if n.Contains(p) {
			return true
		}
	}
	return false
}

// restConn serves buffered bytes first, then the underlying conn.
type restConn struct {
	net.Conn
	rest *bufio.Reader
}

func (c *restConn) Read(p []byte) (int, error) { return c.rest.Read(p) }

// origConn reports the PROXY-declared client address as RemoteAddr.
type origConn struct {
	net.Conn
	orig net.Addr
}

func (c *origConn) RemoteAddr() net.Addr { return c.orig }
