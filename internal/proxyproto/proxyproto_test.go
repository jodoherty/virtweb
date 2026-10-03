package proxyproto

import (
	"bufio"
	"bytes"
	"net"
	"testing"
)

func addr(t *testing.T, r *bytes.Reader, want string) {
	t.Helper()
	a, err := Parse(bufio.NewReader(r))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want != "" {
		if a == nil || a.String() != want {
			t.Fatalf("Parse = %v, want %s", a, want)
		}
	} else if a != nil {
		t.Fatalf("Parse = %v, want nil (UNKNOWN)", a)
	}
}

func TestParseV1(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "" => UNKNOWN (nil addr)
	}{
		{"tcp4", "PROXY TCP4 1.2.3.4 5.6.7.8 1234 80\r\n", "1.2.3.4:1234"},
		{"tcp6", "PROXY TCP6 2001:db8::1 2001:db8::2 1234 80\r\n", "[2001:db8::1]:1234"},
		{"unknown", "PROXY UNKNOWN\r\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			addr(t, bytes.NewReader([]byte(c.in)), c.want)
		})
	}
}

func TestParseV2(t *testing.T) {
	t.Run("inet", func(t *testing.T) {
		var buf bytes.Buffer
		buf.Write(ppv2Sig)
		buf.WriteByte(1) // PROXY
		buf.WriteByte(1) // INET
		buf.WriteByte(1) // TCP
		buf.Write([]byte{0, 12})
		buf.Write([]byte{1, 2, 3, 4})   // src
		buf.Write([]byte{5, 6, 7, 8})   // dst
		buf.Write([]byte{4, 210})       // src port 1234
		buf.Write([]byte{0, 80})        // dst port 80
		addr(t, bytes.NewReader(buf.Bytes()), "1.2.3.4:1234")
	})
	t.Run("inet6", func(t *testing.T) {
		var buf bytes.Buffer
		buf.Write(ppv2Sig)
		buf.WriteByte(1) // PROXY
		buf.WriteByte(2) // INET6
		buf.WriteByte(1) // TCP
		buf.Write([]byte{0, 36})
		src := make([]byte, 16)
		src[15] = 1 // ::1
		dst := make([]byte, 16)
		dst[15] = 2 // ::2
		buf.Write(src)
		buf.Write(dst)
		buf.Write([]byte{4, 210}) // 1234
		buf.Write([]byte{0, 80}) // 80
		addr(t, bytes.NewReader(buf.Bytes()), "[::1]:1234")
	})
	t.Run("local", func(t *testing.T) {
		var buf bytes.Buffer
		buf.Write(ppv2Sig)
		buf.WriteByte(0) // LOCAL
		buf.WriteByte(0) // UNSPEC
		buf.WriteByte(1) // TCP
		buf.Write([]byte{0, 0})
		addr(t, bytes.NewReader(buf.Bytes()), "")
	})
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"no header", []byte("GET / HTTP/1.1\r\n\r\n")},
		{"short", []byte("PROXY ")},
		{"bad v1 transport", []byte("PROXY TCP4 1.2.3.4 5.6.7.8 1234\r\n")},
		{"bad v1 address", []byte("PROXY TCP4 not.an.ip 5.6.7.8 1234 80\r\n")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse(bufio.NewReader(bytes.NewReader(c.in))); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

// fullPipeline dials the listener with a PROXY header prepended and
// returns the connection the server-side handler sees.
func TestListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	l, err := New(ln, []string{"127.0.0.1/32"})
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		c, err := l.Accept()
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		// The original client address from the header is reported as
		// RemoteAddr.
		if got := c.RemoteAddr().String(); got != "1.2.3.4:1234" {
			t.Errorf("RemoteAddr = %s, want 1.2.3.4:1234", got)
		}
		// Bytes written after the header must arrive intact.
		buf := make([]byte, 5)
		if _, err := io_ReadFull(c, buf); err != nil {
			t.Errorf("read payload: %v", err)
		}
		if string(buf) != "hello" {
			t.Errorf("payload = %q, want hello", buf)
		}
		c.Close()
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var hdr bytes.Buffer
	hdr.Write(ppv2Sig)
	hdr.WriteByte(1) // PROXY
	hdr.WriteByte(1) // INET
	hdr.WriteByte(1) // TCP
	hdr.Write([]byte{0, 12})
	hdr.Write([]byte{1, 2, 3, 4})
	hdr.Write([]byte{5, 6, 7, 8})
	hdr.Write([]byte{4, 210}) // 1234
	hdr.Write([]byte{0, 80}) // 80
	if _, err := client.Write(hdr.Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
}

func TestListenerRejectsDisallowedSource(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// 10.0.0.0/8 does not contain the loopback dial source.
	l, err := New(ln, []string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if _, err := l.Accept(); err == nil {
			t.Error("expected rejection of disallowed source")
		}
	}()
	if _, err := net.Dial("tcp", ln.Addr().String()); err != nil {
		t.Fatal(err)
	}
}

// io_ReadFull avoids importing io at top level just for one helper.
func io_ReadFull(c net.Conn, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := c.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
