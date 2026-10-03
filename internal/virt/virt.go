// Package virt defines the domain types and the Backend interface that
// abstracts everything virtweb needs from a libvirt host. API and UI code
// must depend on this interface only, so the virsh-over-SSH implementation
// can be shimmed in tests.
package virt

import (
	"context"
	"io"
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
	VNCPort int    `json:"vncPort,omitempty"` // 0 if not VNC-enabled
}

// Backend is the surface virtweb uses to manage one libvirt host.
type Backend interface {
	// Ping checks that the host and its libvirt are reachable.
	Ping(ctx context.Context) error

	// ListVMs returns all VMs (running and shut off) with details.
	ListVMs(ctx context.Context) ([]VM, error)

	// VMInfo returns the details for one VM.
	VMInfo(ctx context.Context, name string) (VM, error)

	Start(ctx context.Context, name string) error
	Shutdown(ctx context.Context, name string) error // graceful (ACPI)
	Destroy(ctx context.Context, name string) error  // force
	Reboot(ctx context.Context, name string) error

	// Console attaches the VM's serial console to in/out for the lifetime of
	// ctx (or until the console session ends on the remote side).
	Console(ctx context.Context, name string, in io.Reader, out io.Writer) error
}
