// Package virsh implements the virt.Backend interface by invoking the
// remote `virsh` binary over SSH (via sshexec).
package virsh

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.theodohertyfamily.com/virtweb/internal/sshexec"
	"git.theodohertyfamily.com/virtweb/internal/virt"
)

// callTimeout bounds each individual virsh invocation.
const callTimeout = 30 * time.Second

// Backend is a virsh-over-SSH implementation of virt.Backend.
type Backend struct {
	srv virt.Server
	run sshexec.Runner
}

// New creates a Backend for srv using run for SSH access.
func New(srv virt.Server, run sshexec.Runner) *Backend {
	if run == nil {
		run = &sshexec.Std{}
	}
	return &Backend{srv: srv, run: run}
}

// virsh builds and executes `virsh -c <uri> args...` on the remote host.
func (b *Backend) virsh(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	cmd := append([]string{"-c", shellQuote(b.srv.URI)}, args...)
	out, err := b.run.Run(ctx, b.srv.User, b.srv.Host, b.srv.Port,
		"virsh "+shellQuote(strings.Join(cmd, " ")))
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
	wg.Add(2)
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
	wg.Wait()
	if infoErr != nil {
		return vm, infoErr
	}
	_ = ipErr // missing IP info is not fatal
	return vm, nil
}

func (b *Backend) act(ctx context.Context, action, name string) error {
	_, err := b.virsh(ctx, action, name)
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

// Console implements virt.Backend by opening `virsh console` over a
// remote pty (ssh -tt) and wiring the streams through.
func (b *Backend) Console(ctx context.Context, name string, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Hour)
	defer cancel()
	return b.run.Connect(ctx, b.srv.User, b.srv.Host, b.srv.Port, true,
		"virsh -c "+shellQuote(b.srv.URI)+" console "+shellQuote(name), in, out)
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

// parseKiB parses values like "2097152 KiB" into kibibytes.
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
