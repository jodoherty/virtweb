// Package virsh implements the virt.Backend interface by invoking the
// remote `virsh` binary over SSH (via sshexec).
package virsh

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jodoherty/virtweb/internal/sshexec"
	"github.com/jodoherty/virtweb/internal/virt"
)

// callTimeout bounds each individual virsh invocation. Reads (list,
// dominfo, domifaddr, version) are fast and get this bound.
const callTimeout = 30 * time.Second

// actionTimeout bounds lifecycle actions (start, destroy, shutdown,
// reboot). Those block until libvirt reports the domain reached the
// target state, and on a slow emulator (TCG) or a dirty disk that can
// take well over a minute — far longer than a read needs.
const actionTimeout = 90 * time.Second

// Backend is a virsh-over-SSH implementation of virt.Backend.
type Backend struct {
	srv virt.Server
	run sshexec.Runner
	// sem bounds the number of concurrent ssh invocations for this
	// host. ListVMs fans out one vMInfo (several ssh calls) per VM, so
	// without the bound a host with many VMs would spawn an unbounded
	// burst of ssh processes per page load.
	sem chan struct{}
}

// New creates a Backend for srv using run for SSH access.
func New(srv virt.Server, run sshexec.Runner) *Backend {
	if run == nil {
		run = &sshexec.Std{}
	}
	return &Backend{srv: srv, run: run, sem: make(chan struct{}, 8)}
}

// virsh builds and executes `virsh -c <uri> args...` on the remote
// host within callTimeout.
func (b *Backend) virsh(ctx context.Context, args ...string) (string, error) {
	return b.virshWith(ctx, callTimeout, args...)
}

// virshWith is the shared implementation; callers pick the bound that
// fits the operation (reads vs blocking lifecycle actions). At most
// len(sem) ssh processes run for this host at once.
func (b *Backend) virshWith(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	select {
	case b.sem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-b.sem }()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := []string{"virsh", shellQuote("-c"), shellQuote(b.srv.URI)}
	for _, a := range args {
		cmd = append(cmd, shellQuote(a))
	}
	out, err := b.run.Run(ctx, b.srv.User, b.srv.Host, b.srv.Port,
		strings.Join(cmd, " "))
	if err != nil {
		return "", err
	}
	return out, nil
}

// Ping implements virt.Backend.
func (b *Backend) Ping(ctx context.Context) error {
	_, err := b.virsh(ctx, "version")
	return err
}

// ListVMs implements virt.Backend.
func (b *Backend) ListVMs(ctx context.Context) ([]virt.VM, error) {
	out, err := b.virsh(ctx, "list", "--all")
	if err != nil {
		return nil, err
	}
	names, states := parseListAll(out)
	vms := make([]virt.VM, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			vm, err := b.vMInfo(ctx, name)
			if err != nil {
				vm = virt.VM{Name: name, State: states[i]}
			}
			vms[i] = vm
		}(i, name)
	}
	wg.Wait()
	return vms, nil
}

// VMInfo implements virt.Backend.
func (b *Backend) VMInfo(ctx context.Context, name string) (virt.VM, error) {
	return b.vMInfo(ctx, name)
}

func (b *Backend) vMInfo(ctx context.Context, name string) (virt.VM, error) {
	vm := virt.VM{Name: name}
	var wg sync.WaitGroup
	var infoErr, ipErr error
	wg.Add(3)
	go func() {
		defer wg.Done()
		out, err := b.virsh(ctx, "dominfo", name)
		if err != nil {
			infoErr = err
			return
		}
		parseDomInfo(&vm, out)
	}()
	go func() {
		defer wg.Done()
		out, err := b.virsh(ctx, "domifaddr", name)
		if err != nil {
			ipErr = err
			return
		}
		vm.IP = parseDomIfAddr(out)
	}()
	// domdisplay results are collected in locals (not written to vm
	// directly) so the goroutine does not race parseDomInfo's write to
	// vm.VNCPort; they are applied after the join below.
	var displayProto, displayAddr string
	go func() {
		defer wg.Done()
		// domdisplay only works on running domains and returns the
		// primary display address, e.g. "spice://127.0.0.1:5900" or
		// "vnc://host:5900". Auto-ports are assigned at boot, so
		// this is the main way to learn the live one.
		out, err := b.virsh(ctx, "domdisplay", name)
		if err != nil {
			return // not fatal: no display / VM not running
		}
		displayProto, displayAddr = parseDomDisplay(out)
	}()
	wg.Wait()
	if infoErr != nil {
		return vm, infoErr
	}
	_ = ipErr // missing IP info is not fatal
	switch displayProto {
	case "spice":
		vm.SpiceAddr = displayAddr
	case "vnc":
		if _, port, ok := strings.Cut(displayAddr, ":"); ok {
			if p, _ := strconv.Atoi(port); p > 0 {
				vm.VNCPort = p
			}
		}
		// With an auto-port display (port='-1') domdisplay reports the
		// configured placeholder (0), not the assigned port: the live
		// XML carries the real one.
		if vm.VNCPort <= 0 && vm.Running {
			if out, err := b.virsh(ctx, "dumpxml", name); err == nil {
				if p := parseGraphicsPort(out); p > 0 {
					vm.VNCPort = p
				}
			}
		}
	}
	return vm, nil
}

func (b *Backend) act(ctx context.Context, action, name string) error {
	_, err := b.virshWith(ctx, actionTimeout, action, name)
	return err
}

// Start implements virt.Backend.
func (b *Backend) Start(ctx context.Context, name string) error { return b.act(ctx, "start", name) }

// Shutdown implements virt.Backend (graceful ACPI shutdown).
func (b *Backend) Shutdown(ctx context.Context, name string) error { return b.act(ctx, "shutdown", name) }

// Destroy implements virt.Backend (immediate power-off).
func (b *Backend) Destroy(ctx context.Context, name string) error { return b.act(ctx, "destroy", name) }

// Reboot implements virt.Backend.
func (b *Backend) Reboot(ctx context.Context, name string) error { return b.act(ctx, "reboot", name) }

// Define implements virt.Backend with `virsh define -`: the XML is
// Define implements virt.Backend. `virsh define` takes a file path
// (not stdin), so the XML is written to a remote temp file over the
// same ssh session (its stdin), defined from it, and removed — one
// command, no scp, nothing left behind on failure.
func (b *Backend) Define(ctx context.Context, xml string) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var out bytes.Buffer
	cmd := "cat > /tmp/virtweb-define.$$.xml && " +
		"virsh " + shellQuote("-c") + " " + shellQuote(b.srv.URI) +
		" define /tmp/virtweb-define.$$.xml; rc=$?; " +
		"rm -f /tmp/virtweb-define.$$.xml; exit $rc"
	err := b.run.Connect(ctx, b.srv.User, b.srv.Host, b.srv.Port, false,
		cmd, strings.NewReader(xml), &out)
	if err != nil {
		// sshexec.Connect wires virsh's stderr into out; surface it so
		// e.g. "domain with the same name already defined" is visible.
		if msg := strings.TrimSpace(out.String()); msg != "" {
			return fmt.Errorf("virsh define: %v: %s", err, msg)
		}
		return err
	}
	return nil
}

// Undefine implements virt.Backend with `virsh undefine`.
func (b *Backend) Undefine(ctx context.Context, name string) error {
	_, err := b.virsh(ctx, "undefine", name)
	return err
}

// DumpXML implements virt.Backend with `virsh dumpxml`.
func (b *Backend) DumpXML(ctx context.Context, name string) (string, error) {
	return b.virsh(ctx, "dumpxml", name)
}

// Console implements virt.Backend by opening `virsh console` over a
// remote pty (ssh -tt) and wiring the streams through.
func (b *Backend) Console(ctx context.Context, name string, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Hour)
	defer cancel()
	return b.run.Connect(ctx, b.srv.User, b.srv.Host, b.srv.Port, true,
		"virsh -c "+shellQuote(b.srv.URI)+" console "+shellQuote(name), in, out)
}

// Tunnel implements virt.Backend as a local stdio TCP tunnel
// (ssh -N -W target); target is "host:port" as reachable from the
// remote host.
func (b *Backend) Tunnel(ctx context.Context, name, target string, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Hour)
	defer cancel()
	return b.run.Tunnel(ctx, b.srv.User, b.srv.Host, b.srv.Port, target, in, out)
}

// parseListAll parses the output of `virsh list --all`.
func parseListAll(out string) (names []string, states []string) {
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// header row and the dashes separator
		if i < 2 && (strings.HasPrefix(trimmed, "Id") || strings.HasPrefix(trimmed, "-")) {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 3 {
			continue
		}
		names = append(names, fields[1])
		states = append(states, strings.Join(fields[2:], " "))
	}
	return
}

// parseDomInfo fills VM fields from `virsh dominfo` output.
func parseDomInfo(vm *virt.VM, out string) {
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "CPU(s)":
			vm.VCPUs, _ = strconv.Atoi(val)
		case "Max memory", "Maximum memory":
			if kib, err := parseKiB(val); err == nil {
				vm.Memory = kib / 1024
			}
		case "State":
			vm.State = val
			vm.Running = strings.HasPrefix(val, "running") ||
				strings.HasPrefix(val, "paused") ||
				strings.HasPrefix(val, "blocked")
		case "VNC":
			// e.g. "192.168.0.10:5900 (id 0)"
			if hostport := strings.SplitN(strings.Fields(val)[0], ":", 2); len(hostport) == 2 {
				vm.VNCPort, _ = strconv.Atoi(hostport[1])
			}
		}
	}
}

// parseDomIfAddr returns the first IPv4 address seen in `virsh domifaddr`
// output, or "" if none.
func parseDomIfAddr(out string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || strings.HasPrefix(line, "Name") {
			continue
		}
		if fields[2] == "ipv4" && strings.Contains(fields[3], ".") {
			return strings.SplitN(fields[3], "/", 2)[0] // strip CIDR mask
		}
	}
	return ""
}

// parseDomDisplay parses `virsh domdisplay` output ("spice://host:port"
// or "vnc://host:port") into (protocol, "host:port").
func parseDomDisplay(out string) (string, string) {
	out = strings.TrimSpace(out)
	proto, rest, ok := strings.Cut(out, "://")
	if !ok || proto == "" {
		return "", ""
	}
	// strip any trailing path/query (rare, e.g. tls-channel=...)
	if i := strings.IndexAny(rest, " /?"); i >= 0 {
		rest = rest[:i]
	}
	return proto, rest
}

// parseGraphicsPort extracts the VNC port from live domain XML
// (<graphics type='vnc' port='N' .../>); 0 if absent.
func parseGraphicsPort(xml string) int {
	for _, line := range strings.Split(xml, "\n") {
		if !strings.Contains(line, "type='vnc'") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(f, "port='"); ok {
				p, _ := strconv.Atoi(strings.TrimSuffix(v, "'"))
				return p
			}
		}
	}
	return 0
}

func parseKiB(val string) (int, error) {
	fields := strings.Fields(val)
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty")
	}
	return strconv.Atoi(fields[0])
}

// shellQuote quotes s for safe use in a remote POSIX shell.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	need := false
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.IndexRune("@%+=:,./_-", c) >= 0) {
			need = true
			break
		}
	}
	if !need {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
