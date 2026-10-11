// Command virtweb is a web-based management interface for libvirt hosts.
//
// It builds to a single binary with the web frontend and the vendored
// terminal/VNC libraries embedded via go:embed.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jodoherty/virtweb/internal/api"
	"github.com/jodoherty/virtweb/internal/proxyproto"
	"github.com/jodoherty/virtweb/internal/sshexec"
	"github.com/jodoherty/virtweb/internal/stub"
	"github.com/jodoherty/virtweb/internal/store"
	"github.com/jodoherty/virtweb/internal/virt"
	"github.com/jodoherty/virtweb/internal/virsh"
	"github.com/jodoherty/virtweb/internal/webui"
)

//go:embed web
var webFS embed.FS

// First-party forks (see third-party/*/PROVENANCE.md):
//   xterm.js 6.0.0 + addon-fit 0.11.0 (MIT)               -> serial console
//   noVNC 1.7.0 (MPL-2.0) fork                            -> VNC viewer
//   spice-html5 fork (LGPL-3.0), transport forked by us   -> SPICE viewer
//
//go:embed third-party/xtermjs
var xtermFS embed.FS
//
//go:embed third-party/novnc
var novncFS embed.FS
//
//go:embed third-party/spice-html5-fork
var spiceFS embed.FS

// version is stamped at build time (see Makefile: -ldflags -X main.version).
// Defaults to "dev" for plain `go build`.
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	addr := flag.String("addr", "127.0.0.1:8080", "address to listen on")
	dataDir := flag.String("data-dir", defaultDataDir(), "directory for persistent data (servers.json)")
	backendKind := flag.String("backend", "ssh", "backend: ssh (production) or stub (offline demo/test mode)")
	selfSigned := flag.String("self-signed", "", "generate a self-signed server certificate with SANs for these hosts (comma-separated DNS names and/or IPs, e.g. 'localhost,127.0.0.1'); exactly one of -self-signed and -tls-cert must be given — development/testing only")
	tlsCert := flag.String("tls-cert", "", "PEM server certificate file; exactly one of -self-signed and -tls-cert must be given (with -tls-key)")
	tlsKey := flag.String("tls-key", "", "PEM server private key file, given together with -tls-cert")
	clientCA := flag.String("client-ca", "", "PEM CA bundle; enables mTLS (client certificates are required and verified). Client auth is the default: either this or -no-auth must be given")
	noAuth := flag.Bool("no-auth", false, "run WITHOUT client certificate authentication (mTLS). An explicit opt-out: the default requires -client-ca, so passing this is how you deliberately serve without client auth")
	proxyProto := flag.Bool("proxy-protocol", false, "expect a PROXY protocol (v1/v2) header on each connection, e.g. behind a Traefik L4 pass-through router")
	proxyFrom := flag.String("proxy-from", "", "with -proxy-protocol: comma-separated source IPs/CIDRs of the proxy; connections from any other source are rejected")
	hostKeyChecking := flag.String("host-key-checking", "accept-new", "ssh -o StrictHostKeyChecking value: accept-new (default: trust first contact, verify afterwards), yes, no (known_hosts only), or off")
	keepalive := flag.Duration("keepalive", api.DefaultKeepalive, "HTTP/1.1 session keepalive window: probe a silent stream session with a ping after this much silence (0 disables); keep it well under your reverse proxy's idle/read timeout. HTTP/2 sessions run with the window zero (streams cannot enforce deadlines); liveness there is the transport's job")
	flag.Parse()

	if *showVersion {
		fmt.Println("virtweb " + version)
		return
	}
	// The server certificate must be chosen explicitly: exactly one of
	// -self-signed and -tls-cert/-tls-key. There is no silent
	// fallback (a self-signed certificate is a security posture, not a
	// convenience default).
	if err := validateCertFlags(*selfSigned, *tlsCert, *tlsKey); err != nil {
		log.Fatal(err)
	}
	// Client authentication is the default; the choice must be stated
	// on the command line (so it is visible in the process listing and
	// the logs), not be a silent fallback.
	if err := validateAuthFlags(*clientCA, *noAuth); err != nil {
		log.Fatal(err)
	}
	if *proxyProto && *proxyFrom == "" {
		log.Fatal("-proxy-protocol requires -proxy-from (the proxy's source IP/CIDR); without it, any peer on the network could forge the PROXY header")
	}
	switch *hostKeyChecking {
	case "accept-new", "yes", "no", "off":
	default:
		log.Fatalf("-host-key-checking must be one of: accept-new, yes, no, off (got %q)", *hostKeyChecking)
	}

	// Guarantee the HTTP/2 extended-CONNECT routing before anything
	// listens (see ensureExtendedConnect).
	ensureExtendedConnect()

	// Frontend is served straight from the embedded filesystem.
	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}

	st, err := store.New(filepath.Join(*dataDir, "servers.json"))
	if err != nil {
		log.Fatal(err)
	}

	var factory api.BackendFactory
	switch *backendKind {
	case "ssh":
		factory = &backendFactory{ssh: &sshexec.Std{StrictHostKeyChecking: *hostKeyChecking}}
	case "stub":
		sc, err := stub.ParseScenario(os.Getenv("STUB_SCENARIO"))
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("using stub backend (offline demo mode; scenario %q)", string(sc))
		factory = &stub.Factory{Scenario: sc}
	default:
		log.Fatalf("unknown -backend %q (want ssh or stub)", *backendKind)
	}

	apiSrv := api.New(st, factory, api.WithKeepalive(*keepalive))
	ui, err := webui.New(st, factory, webRoot)
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/", apiSrv.Routes())
	mux.Handle("/static/", webui.Static(webRoot))
	mux.Handle("/", ui.Routes())
	for prefix, fsys := range map[string]embed.FS{"/xtermjs/": xtermFS, "/novnc/": novncFS, "/spice/": spiceFS} {
		sub, err := subVendored(fsys, prefix)
		if err != nil {
			log.Fatal(err)
		}
		mux.Handle(prefix, webui.NoCacheFS(sub, prefix))
	}

	// Cross-origin protection (CSRF): non-safe (POST/DELETE/PUT/...) 
	// cross-origin browser requests are rejected (Sec-Fetch-Site / 
	// Origin check, stdlib net/http). Safe methods are always allowed, 
	// and non-browser clients (curl, node, API automation) send none of
	// those headers, so they pass.
	cop := http.NewCrossOriginProtection()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           securityHeaders(cop.Handler(mux)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Listener pipeline: TCP accept -> [PROXY header] -> [TLS/mTLS] ->
	// HTTP. virtweb always serves TLS: the WebSocket transport
	// requires it (browsers negotiate HTTP/2 over ALPN only on TLS
	// listeners, and Go's net/http enables h2 automatically on the TLS
	// server). The PROXY header rides outside TLS (the L4 proxy
	// prepends it to the raw stream); TLS (and mTLS client-cert auth)
	// is terminated by virtweb itself.
	cert, err := serverCert(*selfSigned, *tlsCert, *tlsKey)
	if err != nil {
		log.Fatal(err)
	}
	cfg, mTLS, err := tlsConfig(cert, *clientCA)
	if err != nil {
		log.Fatal(err)
	}
	if !mTLS && *selfSigned != "" {
		log.Printf("WARNING: serving with a SELF-SIGNED certificate for %s (generated by -self-signed). Development/testing only — production must use real certificates (e.g. Let's Encrypt; see doc/USAGE.md).", *selfSigned)
	}
	if !mTLS {
		log.Printf("WARNING: -no-auth: client certificate authentication is DISABLED — any TLS client can reach the API. Production must pass -client-ca to require mTLS.")
	}
	srv.TLSConfig = cfg

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	if *proxyProto {
		ln, err = proxyproto.New(ln, strings.Split(*proxyFrom, ","))
		if err != nil {
			log.Fatal(err)
		}
	}
	if *clientCA != "" {
		log.Printf("virtweb %s listening on https://%s (mTLS: client certificates required; data dir: %s)", version, *addr, *dataDir)
	} else {
		log.Printf("virtweb %s listening on https://%s (no client auth, -no-auth; data dir: %s)", version, *addr, *dataDir)
	}
	go func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// securityHeaders adds the response hardening headers virtweb wants on
// every response:
//   nosniff:        browsers must not re-sniff content types
//   X-Frame-Options + CSP frame-ancestors: never framed (clickjacking)
//   Referrer-Policy: server ids live in URL paths — never leak them
//   CSP: all scripts/styles are same-origin files; the page uses no
//        inline scripts (data-confirm + ui.js instead of onsubmit) and
//        no eval; img-src allows data:/blob: for the VNC/SPICE viewers'
//        cursor and playback images.
// ensureExtendedConnect re-executes the process with
// GODEBUG=http2xconnect=1 when the setting is not already present.
//
// The stdlib HTTP/2 server does not route extended CONNECT (RFC 8441)
// by default: it is gated behind that GODEBUG setting, read once at
// process start, and without it the server never advertises the
// capability — so browsers cannot carry their WebSocket sessions on
// the h2 connection and every session degrades to the HTTP/1.1
// Upgrade leg (which works, but with keepalive-driven liveness and
// per-origin connection budgets instead of transport-level
// keepalive). The setting cannot be flipped after net/http's init,
// and h2 is the designed primary path, so the binary guarantees its
// own invariant: one re-exec with the setting appended to GODEBUG,
// then main runs as usual. (The h1 warning on any session that still
// lands on the Upgrade leg is the canary if this ever regresses.)
func ensureExtendedConnect() {
	const setting = "http2xconnect=1"
	godebug := os.Getenv("GODEBUG")
	for _, part := range strings.Split(godebug, ",") {
		if strings.TrimSpace(part) == setting {
			return // already set (inherited or explicit)
		}
	}

	self, err := os.Executable()
	if err != nil {
		log.Fatal("cannot re-exec for HTTP/2 extended CONNECT: " + err.Error())
	}
	cmd := exec.Command(self, os.Args[1:]...)
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GODEBUG=") {
			env = append(env, kv)
		}
	}
	if godebug != "" {
		env = append(env, "GODEBUG="+godebug+","+setting)
	} else {
		env = append(env, "GODEBUG="+setting)
	}
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	log.Printf("virtweb: re-executing with GODEBUG=%s (HTTP/2 extended CONNECT routing)",
		"http2xconnect=1")
	if err := cmd.Start(); err != nil {
		log.Fatal("re-exec for HTTP/2 extended CONNECT failed: " + err.Error())
	}

	// Forward termination signals to the child and exit with its
	// status: the parent exists only to carry the environment.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()
	err = cmd.Wait()
	state := cmd.ProcessState
	if err != nil && state == nil {
		log.Fatal("re-exec'd virtweb: " + err.Error())
	}
	code := 0
	if state != nil && !state.Success() {
		code = 1
		if st, ok := state.Sys().(syscall.WaitStatus); ok && st.Exited() {
			code = st.ExitStatus()
		}
	}
	os.Exit(code)
}

// validateAuthFlags enforces the secure-by-default posture: client
// certificate authentication is the default, so exactly one of
// -client-ca (mTLS) and -no-auth (explicit opt-out) must be given.
func validateAuthFlags(clientCA string, noAuth bool) error {
	if clientCA != "" && noAuth {
		return errors.New("-client-ca and -no-auth are mutually exclusive: -client-ca enables mTLS, -no-auth disables client authentication")
	}
	if clientCA == "" && !noAuth {
		return errors.New("client certificate authentication is required: pass -client-ca (PEM CA bundle) for mTLS, or -no-auth to explicitly run without client authentication")
	}
	return nil
}

func securityHeaders(next http.Handler) http.Handler {
	csp := "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data: blob:; media-src 'self' blob:; object-src 'none'; " +
		"base-uri 'self'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}

// validateCertFlags enforces the explicit server-certificate choice:
// exactly one of -self-signed (generate a self-signed certificate with
// the given SANs) and -tls-cert/-tls-key (a PEM pair). Neither, or
// both, is a configuration error — there is no silent fallback.
func validateCertFlags(selfSigned, certFile, keyFile string) error {
	switch {
	case selfSigned == "" && certFile == "" && keyFile == "":
		return errors.New("no server certificate: pass -self-signed <hosts> (generate a self-signed certificate) or -tls-cert/-tls-key (PEM pair)")
	case selfSigned != "" && certFile != "":
		return errors.New("-self-signed and -tls-cert are mutually exclusive: give one self-signed host list, or one PEM pair")
	case certFile != "" && keyFile == "":
		return errors.New("-tls-cert requires -tls-key")
	case certFile == "" && keyFile != "":
		return errors.New("-tls-key requires -tls-cert")
	}
	return nil
}

// serverCert resolves the server certificate from the validated
// flags: a self-signed certificate generated from the -self-signed
// host list, or the PEM pair named by -tls-cert/-tls-key.
func serverCert(selfSigned, certFile, keyFile string) (tls.Certificate, error) {
	if selfSigned != "" {
		return selfSignedCert(selfSigned)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load -tls-cert/-tls-key: %w", err)
	}
	return cert, nil
}

// selfSignedCert generates a self-signed server certificate with SANs
// for the -self-signed host list (comma-separated DNS names and/or
// IPs). It exists for local development/testing under the
// TLS-required design and to exercise the real TLS/h2 code path;
// production always supplies real certificates via -tls-cert/-tls-key.
func selfSignedCert(spec string) (tls.Certificate, error) {
	var dnsNames []string
	var ipAddrs []net.IP
	for _, h := range strings.Split(spec, ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			ipAddrs = append(ipAddrs, ip)
		} else {
			dnsNames = append(dnsNames, h)
		}
	}
	if len(dnsNames) == 0 && len(ipAddrs) == 0 {
		return tls.Certificate{}, fmt.Errorf("-self-signed: no usable hosts in %q (give DNS names and/or IPs)", spec)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "virtweb (self-signed)"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ipAddrs,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// tlsConfig builds the server TLS config around the resolved server
// certificate. When caFile is non-empty, mTLS is enabled: clients must
// present a certificate signed by one of the CAs in caFile
// (RequireAndVerifyClientCert). mTLS reports true when enabled.
func tlsConfig(cert tls.Certificate, caFile string) (*tls.Config, bool, error) {
	cfg := &tls.Config{
		MinVersion:    tls.VersionTLS12,
		Certificates:  []tls.Certificate{cert},
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, false, fmt.Errorf("read -client-ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, false, errors.New("no CA certificates found in -client-ca")
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		return cfg, true, nil
	}
	return cfg, false, nil
}

// subVendored strips the third-party/<dir> prefix from an embedded
// library tree so /xtermjs/css/xterm.css maps into the tree root.
var vendoredDirs = map[string]string{
	"/xtermjs/": "third-party/xtermjs",
	"/novnc/":   "third-party/novnc",
	"/spice/":   "third-party/spice-html5-fork",
}

func subVendored(fsys embed.FS, urlPrefix string) (fs.FS, error) {
	rel, ok := vendoredDirs[urlPrefix]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return fs.Sub(fsys, rel)
}

// backendFactory builds virsh-over-SSH backends sharing one SSH runner.
type backendFactory struct {
	ssh *sshexec.Std
}

func (f *backendFactory) NewBackend(srv virt.Server) virt.Backend {
	// Per-server auth: the shared Std carries only the operator-level
	// ssh policy (host-key checking); credentials are server-scoped so
	// each backend execs ssh with that server's key/password only.
	run := &sshexec.Std{
		StrictHostKeyChecking: f.ssh.StrictHostKeyChecking,
		Key:                   srv.Key,
		Password:              srv.Password,
	}
	return virsh.New(srv, run)
}

// defaultDataDir returns the default location for virtweb data.
func defaultDataDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".virtweb")
	}
	return ".virtweb"
}
