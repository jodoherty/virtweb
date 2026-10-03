package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jodoherty/virtweb/internal/proxyproto"
)

type certFiles struct {
	ca     string // CA cert PEM
	server string // server cert + key PEM (one file)
	client string // client cert + key PEM (one file)
}

// genCertFiles mints a CA, a server cert (SAN 127.0.0.1) and a client
// cert, written as PEM files in a temp dir.
func genCertFiles(t *testing.T) certFiles {
	t.Helper()
	dir := t.TempDir()
	now := time.Now()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "virtweb test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	type pemBlock struct {
		typ string
		b   []byte
	}
	writePEM := func(name string, blocks ...pemBlock) string {
		t.Helper()
		var buf bytes.Buffer
		for _, blk := range blocks {
			if err := pem.Encode(&buf, &pem.Block{Type: blk.typ, Bytes: blk.b}); err != nil {
				t.Fatal(err)
			}
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	keyPEM := func(k *ecdsa.PrivateKey) []byte {
		b, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	caPEM := writePEM("ca.pem", pemBlock{"CERTIFICATE", der})

	sk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	sDer, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}, caCert, &sk.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	serverPEM := writePEM("server.pem", pemBlock{"CERTIFICATE", sDer}, pemBlock{"EC PRIVATE KEY", keyPEM(sk)})

	ck, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	cDer, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "virtweb-test-client"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, caCert, &ck.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	clientPEM := writePEM("client.pem", pemBlock{"CERTIFICATE", cDer}, pemBlock{"EC PRIVATE KEY", keyPEM(ck)})

	return certFiles{ca: caPEM, server: serverPEM, client: clientPEM}
}

// proxyV2Header builds a PROXY protocol v2 header announcing src
// 9.9.9.9:999 -> the listener address.
func proxyV2Header(t *testing.T, dst string) []byte {
	t.Helper()
	host, port, err := net.SplitHostPort(dst)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.Write(ppv2TestSig)
	buf.WriteByte(1) // PROXY
	buf.WriteByte(1) // INET
	buf.WriteByte(1) // TCP
	buf.Write([]byte{0, 12})
	buf.Write(net.ParseIP("9.9.9.9").To4())
	buf.Write(net.ParseIP(host).To4())
	buf.Write([]byte{byte(999 >> 8), byte(999 & 0xff)})
	buf.Write([]byte{byte(p >> 8), byte(p)})
	return buf.Bytes()
}

var ppv2TestSig = []byte{0x0d, 0x0a, 0x0d, 0x0a, 0x00, 0x0d, 0x0a, 0x51, 0x55, 0x49, 0x54, 0x0a}

// TestMtlsProxyPipeline exercises the full listener stack:
// TCP accept -> PROXY v2 header -> mTLS handshake -> HTTP.
func TestMtlsProxyPipeline(t *testing.T) {
	f := genCertFiles(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pl, err := proxyproto.New(ln, []string{"127.0.0.1/32"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := tlsConfigFromFiles(f.server, f.server, f.ca)
	if err != nil {
		t.Fatal(err)
	}
	var peerCN string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) != 1 {
			t.Error("expected exactly one client certificate")
			return
		}
		peerCN = r.TLS.PeerCertificates[0].Subject.CommonName
		fmt.Fprintln(w, "ok")
	})
	srv := &http.Server{TLSConfig: cfg, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.ServeTLS(pl, "", "") }()
	defer srv.Close()

	caPool := x509.NewCertPool()
	caPEM, err := os.ReadFile(f.ca)
	if err != nil {
		t.Fatal(err)
	}
	if !caPool.AppendCertsFromPEM(caPEM) {
		t.Fatal("no CA cert")
	}
	clientCerts, err := tls.LoadX509KeyPair(f.client, f.client)
	if err != nil {
		t.Fatal(err)
	}
	dialWithProxy := func() net.Conn {
		t.Helper()
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write(proxyV2Header(t, ln.Addr().String())); err != nil {
			t.Fatal(err)
		}
		return c
	}

	// Negative: no client certificate -> the server must reject the
	// connection. (The client's Handshake can appear to complete before
	// the server-side rejection arrives; the failure then surfaces on
	// the first I/O.)
	c := dialWithProxy()
	defer c.Close()
	tc := tls.Client(c, &tls.Config{RootCAs: caPool, ServerName: "127.0.0.1"})
	tc.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tc.Handshake(); err == nil {
		_, _ = tc.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
		resp, _ := io.ReadAll(tc)
		if bytes.Contains(resp, []byte("200 OK")) {
			t.Error("expected the mTLS connection to be rejected without a client certificate")
		}
	}
	if err := tc.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}

	// Positive: with a client cert signed by the CA, the request goes
	// through and the server sees the client certificate.
	c = dialWithProxy()
	defer c.Close()
	tc = tls.Client(c, &tls.Config{RootCAs: caPool, ServerName: "127.0.0.1",
			Certificates: []tls.Certificate{clientCerts}})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("mTLS handshake: %v", err)
	}
	if _, err := tc.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := io.ReadAll(tc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(resp, []byte("HTTP/1.1 200 OK")) || !bytes.Contains(resp, []byte("ok")) {
		t.Errorf("unexpected response: %q", resp)
	}
	if peerCN != "virtweb-test-client" {
		t.Errorf("server saw client cert CN %q", peerCN)
	}
}
