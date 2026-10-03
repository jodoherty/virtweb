// Package virt defines the domain types and the Backend interface that
// abstracts everything virtweb needs from a libvirt host. API and UI code
// must depend on this interface only, so the virsh-over-SSH implementation
// can be shimmed in tests.
package virt

import (
	"context"
	"io"
	"strings"
)

// Server is a libvirt host reachable over SSH.
type Server struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	User string `json:"user"`
	Host string `json:"host"`
	Port int    `json:"port"` // 0 means the default SSH port (22)
	URI  string `json:"uri"`  // libvirt URI, e.g. "qemu:///system"
}

// VM is a virtual machine on a Server.
type VM struct {
	Name    string `json:"name"`
	State   string `json:"state"` // raw libvirt state, e.g. "running", "shut off"
	Running bool   `json:"running"`
	VCPUs   int    `json:"vcpus,omitempty"`
	Memory  int    `json:"memory,omitempty"` // MiB
	IP      string `json:"ip,omitempty"`
	SpiceAddr string `json:"spiceAddr,omitempty"` // SPICE "host:port" (from domdisplay); "" if none/absent
	VNCPort int    `json:"vncPort,omitempty"`     // 0 if not VNC-enabled
}

// DomainName extracts the domain name from a libvirt domain XML
// document: the first <name> element, which in domain XML always comes
// right after the <domain> tag — before any other <name> elements
// (interface names, etc.). Returns "" when absent.
func DomainName(xml string) string {
	i := strings.Index(xml, "<name>")
	if i < 0 {
		return ""
	}
	rest := xml[i+len("<name>"):]
	if j := strings.Index(rest, "</name>"); j >= 0 {
		return strings.TrimSpace(rest[:j])
	}
	return ""
}

// Backend is the surface virtweb uses to manage one libvirt host.
type Backend interface {
	// Ping checks that the host and its libvirt are reachable.
	Ping(ctx context.Context) error

	// ListVMs returns all VMs (running and shut off) with details.
	ListVMs(ctx context.Context) ([]VM, error)

	// VMInfo returns the details for one VM.
	VMInfo(ctx context.Context, name string) (VM, error)

	// DumpXML returns the domain's current libvirt XML (virsh dumpxml).
	DumpXML(ctx context.Context, name string) (string, error)

	// Define creates a persistent domain from a libvirt domain XML
	// document (the caller has already validated that it names a
	// domain). The domain starts shut off; Start it afterwards.
	Define(ctx context.Context, xml string) error

	// Undefine removes a domain definition (virsh undefine); it fails
	// if the domain is running — destroy it first.
	Undefine(ctx context.Context, name string) error

	Start(ctx context.Context, name string) error
	Shutdown(ctx context.Context, name string) error // graceful (ACPI)
	Destroy(ctx context.Context, name string) error  // force
	Reboot(ctx context.Context, name string) error

	// Console attaches the VM's serial console to in/out for the lifetime of
	// ctx (or until the console session ends on the remote side).
	Console(ctx context.Context, name string, in io.Reader, out io.Writer) error

	// Tunnel opens a TCP tunnel to target ("host:port") on the remote
	// host, wiring in/out for the lifetime of ctx. The VM name identifies
	// the session (logging, one session per (VM, target)).
	Tunnel(ctx context.Context, name, target string, in io.Reader, out io.Writer) error
}
