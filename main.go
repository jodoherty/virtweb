// Command virtweb is a web-based management interface for libvirt hosts.
//
// It builds to a single binary with the web frontend embedded via go:embed.
package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"git.theodohertyfamily.com/virtweb/internal/api"
	"git.theodohertyfamily.com/virtweb/internal/sshexec"
	"git.theodohertyfamily.com/virtweb/internal/store"
	"git.theodohertyfamily.com/virtweb/internal/virt"
	"git.theodohertyfamily.com/virtweb/internal/virsh"
)

//go:embed all:web
var webFS embed.FS

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "address to listen on")
	dataDir := flag.String("data-dir", defaultDataDir(), "directory for persistent data (servers.json)")
	flag.Parse()

	// Frontend is served straight from the embedded filesystem.
	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}

	st, err := store.New(filepath.Join(*dataDir, "servers.json"))
	if err != nil {
		log.Fatal(err)
	}
	apiSrv := api.New(st, &backendFactory{ssh: &sshexec.Std{}})

	mux := http.NewServeMux()
	mux.Handle("/api/", apiSrv.Routes())
	mux.Handle("/", http.FileServerFS(webRoot))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("virtweb listening on http://%s (data dir: %s)", *addr, *dataDir)
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