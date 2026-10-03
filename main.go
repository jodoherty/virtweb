// Command virtweb is a web-based management interface for libvirt hosts.
//
// It builds to a single binary with the web frontend and the vendored
// terminal/VNC libraries embedded via go:embed.
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jodoherty/virtweb/internal/api"
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
	flag.Parse()

	if *showVersion {
		fmt.Println("virtweb " + version)
		return
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
		factory = &backendFactory{ssh: &sshexec.Std{}}
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

	apiSrv := api.New(st, factory)
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
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("virtweb %s listening on http://%s (data dir: %s)", version, *addr, *dataDir)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
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
