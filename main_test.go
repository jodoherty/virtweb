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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	cert, err := serverCert("", f.server, f.server)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := tlsConfig(cert, f.ca)
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

func TestValidateAuthFlags(t *testing.T) {
	cases := []struct {
		name     string
		clientCA string
		noAuth   bool
		wantErr  bool
	}{
		{"mTLS (the default posture)", "ca.pem", false, false},
		{"explicit opt-out", "", true, false},
		{"refused: no auth posture", "", false, true},
		{"refused: both", "ca.pem", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateAuthFlags(c.clientCA, c.noAuth)
			if (err != nil) != c.wantErr {
				t.Fatalf("validateAuthFlags(%q, %v) error = %v; want error %v",
					c.clientCA, c.noAuth, err, c.wantErr)
			}
		})
	}
}

func TestValidateCertFlags(t *testing.T) {
	cases := []struct {
		name       string
		selfSigned string
		cert, key  string
		wantErr    bool
	}{
		{"self-signed only", "localhost,127.0.0.1", "", "", false},
		{"PEM pair only", "", "tls.crt", "tls.key", false},
		{"refused: neither", "", "", "", true},
		{"refused: both", "localhost", "tls.crt", "tls.key", true},
		{"refused: cert without key", "", "tls.crt", "", true},
		{"refused: key without cert", "", "", "tls.key", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateCertFlags(c.selfSigned, c.cert, c.key)
			if (err != nil) != c.wantErr {
				t.Fatalf("validateCertFlags(%q, %q, %q) error = %v; want error %v",
					c.selfSigned, c.cert, c.key, err, c.wantErr)
			}
		})
	}
}

// TestSelfSignedCert: the generated certificate must verify for the
// hosts named in the spec (DNS via the name, IPs via the IP SANs).
func TestSelfSignedCert(t *testing.T) {
	cert, err := selfSignedCert("virtweb.example.test, 127.0.0.1, ::1")
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	for _, dns := range leaf.DNSNames {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: dns}); err != nil {
			t.Errorf("DNS verification for %q: %v", dns, err)
		}
	}
	for _, ip := range leaf.IPAddresses {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: ip.String()}); err != nil {
			t.Errorf("IP verification for %v: %v", ip, err)
		}
	}
	// Unnamed hosts must not verify.
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "elsewhere.test"}); err == nil {
		t.Error("verification unexpectedly succeeded for an unnamed host")
	}
	for _, spec := range []string{"", " , ,"} {
		if _, err := selfSignedCert(spec); err == nil {
			t.Errorf("selfSignedCert(%q): expected an error for a spec without usable hosts", spec)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	res, err := http.Get(tsURL(t, h))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for k, v := range want {
		if got := res.Header.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
	if csp := res.Header.Get("Content-Security-Policy"); csp == "" {
		t.Error("missing Content-Security-Policy header")
	} else {
		for _, frag := range []string{"default-src 'self'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, frag) {
				t.Errorf("CSP %q missing fragment %q", csp, frag)
			}
		}
	}
}

// tsURL serves h on a throwaway server and returns its base URL.
func tsURL(t *testing.T, h http.Handler) string {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts.URL
}

// TestCrossOriginProtection verifies the CSRF guard wired into main:
// non-safe cross-origin browser requests are rejected, same-origin and
// non-browser requests pass, and safe methods are always allowed.
func TestCrossOriginProtection(t *testing.T) {
	cop := http.NewCrossOriginProtection()
	var served bool
	h := cop.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	}))
	ts := httptest.NewServer(h)
	defer ts.Close()

	do := func(method, origin, sfs string) int {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+"/x", nil)
		if err != nil {
			t.Fatal(err)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if sfs != "" {
			req.Header.Set("Sec-Fetch-Site", sfs)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	// Cross-origin browser POST -> 403, and the handler never runs.
	if code := do("POST", "https://evil.example", "cross-site"); code != http.StatusForbidden {
		t.Errorf("cross-origin POST status = %d, want 403", code)
	}
	if served {
		t.Error("handler served a cross-origin POST; it must be rejected")
	}

	// Same-origin browser POST -> allowed.
	if code := do("POST", ts.URL, "same-origin"); code != http.StatusOK {
		t.Errorf("same-origin POST status = %d, want 200", code)
	}

	// Non-browser client (no Sec-Fetch-Site/Origin) -> allowed.
	if code := do("POST", "", ""); code != http.StatusOK {
		t.Errorf("headerless POST status = %d, want 200", code)
	}

	// Safe methods are always allowed, even cross-origin.
	if code := do("GET", "https://evil.example", "cross-site"); code != http.StatusOK {
		t.Errorf("cross-origin GET status = %d, want 200", code)
	}
}
