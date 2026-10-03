// Command virtweb is a web-based management interface for libvirt hosts.
//
// It builds to a single binary with the web frontend and the vendored
// terminal/VNC libraries embedded via go:embed.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
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
	tlsEnable := flag.Bool("tls", false, "serve HTTPS (requires -tls-cert and -tls-key)")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file (PEM)")
	tlsKey := flag.String("tls-key", "", "TLS private key file (PEM)")
	clientCA := flag.String("client-ca", "", "PEM CA bundle; requires -tls and enables mTLS (client certificates are required and verified)")
	proxyProto := flag.Bool("proxy-protocol", false, "expect a PROXY protocol (v1/v2) header on each connection, e.g. behind a Traefik L4 pass-through router")
	proxyFrom := flag.String("proxy-from", "", "with -proxy-protocol: comma-separated source IPs/CIDRs of the proxy; connections from any other source are rejected")
	hostKeyChecking := flag.String("host-key-checking", "accept-new", "ssh -o StrictHostKeyChecking value: accept-new (default: trust first contact, verify afterwards), yes, no (known_hosts only), or off")
	keepalive := flag.Duration("keepalive", api.DefaultKeepalive, "stream keepalive: emit an idle-keepalive frame on console/tunnel streams after this much silence (0 disables); keep it well under your reverse proxy's idle/read timeout")
	flag.Parse()

	if *showVersion {
		fmt.Println("virtweb " + version)
		return
	}

	if *clientCA != "" && !*tlsEnable {
		log.Fatal("-client-ca requires -tls")
	}
	if *tlsEnable && (*tlsCert == "" || *tlsKey == "") {
		log.Fatal("-tls requires -tls-cert and -tls-key")
	}
	if *proxyProto && *proxyFrom == "" {
		log.Fatal("-proxy-protocol requires -proxy-from (the proxy's source IP/CIDR); without it, any peer on the network could forge the PROXY header")
	}
	switch *hostKeyChecking {
	case "accept-new", "yes", "no", "off":
	default:
		log.Fatalf("-host-key-checking must be one of: accept-new, yes, no, off (got %q)", *hostKeyChecking)
	}

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
		mux.Handle(prefix, http.StripPrefix(prefix, http.FileServerFS(sub)))
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
	// HTTP. The PROXY header rides outside TLS (the L4 proxy prepends
	// it to the raw stream); TLS (and mTLS client-cert auth) is
	// terminated by virtweb itself.
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
	if *tlsEnable {
		cfg, err := tlsConfigFromFiles(*tlsCert, *tlsKey, *clientCA)
		if err != nil {
			log.Fatal(err)
		}
		srv.TLSConfig = cfg
		if *clientCA != "" {
			log.Printf("virtweb %s listening on https://%s (mTLS: client certificates required; data dir: %s)", version, *addr, *dataDir)
		} else {
			log.Printf("virtweb %s listening on https://%s (data dir: %s)", version, *addr, *dataDir)
		}
		go func() {
			if err := srv.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
				log.Fatal(err)
			}
		}()
	} else {
		log.Printf("virtweb %s listening on http://%s (data dir: %s)", version, *addr, *dataDir)
		go func() {
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				log.Fatal(err)
			}
		}()
	}

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

// tlsConfigFromFiles builds the server TLS config. When caFile is
// non-empty, mTLS is enabled: clients must present a certificate
// signed by one of the CAs in caFile (RequireAndVerifyClientCert).
func tlsConfigFromFiles(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load -tls-cert/-tls-key: %w", err)
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read -client-ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("no CA certificates found in -client-ca")
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
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
	return virsh.New(srv, f.ssh)
}

// defaultDataDir returns the default location for virtweb data.
func defaultDataDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".virtweb")
	}
	return ".virtweb"
}
