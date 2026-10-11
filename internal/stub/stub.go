// Package stub provides a deterministic fake libvirt backend. It is
// selected at runtime with `virtweb -backend stub` and serves two
// purposes:
//
//  1. Tier 0 of the e2e suite (TESTING.md): a real virtweb binary with
//     the real embedded UI, REST API, console sessions and store, but
//     the backend swapped for this simulation — fast, offline, and
//     reproducible on demand via failure scenarios.
//  2. An offline demo / dry-run mode, so the UI can be explored before
//     any real host is configured.
//
// Output strings deliberately track the shape that the real (virsh)
// backend produces; where they diverge, the real one wins (TESTING.md
// ground rule 1). The stub is the *only* place the intermediate
// "starting" state exists: real libvirt reports a booting domain as
// "running" immediately, but the stub delays the transition so UI code
// that polls state gets to exercise the in-between.
package stub

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/jodoherty/virtweb/internal/virt"
)

// Default timing knobs.
const (
	defaultBootDelay = 500 * time.Millisecond // start → running
	slowBootDelay    = 30 * time.Second       // STUB_SCENARIO=slow-boot
	consoleDropDelay = 200 * time.Millisecond // STUB_SCENARIO=console-drop
)

// Scenario is an injectable failure mode (normally set from the
// STUB_SCENARIO environment variable, read in main).
type Scenario string

// Failure scenarios.
const (
	ScenarioNone        Scenario = ""
	ScenarioFailPing    Scenario = "fail-ping"
	ScenarioSlowBoot    Scenario = "slow-boot"
	ScenarioConsoleDrop Scenario = "console-drop"
	ScenarioSSHDown     Scenario = "ssh-down"
	ScenarioTunnelDrop  Scenario = "tunnel-drop"
	// tunnel-fail: the tunnel dies shortly after the banner with an
	// error (the 1011 close path; the close reason carries this
	// message).
	ScenarioTunnelFail Scenario = "tunnel-fail"
	// console-fail: the console session fails right after attach with
	// an error (the 1011 close path for consoles; everything else —
	// list, start — works).
	ScenarioConsoleFail Scenario = "console-fail"
)

// ParseScenario validates a STUB_SCENARIO value.
func ParseScenario(s string) (Scenario, error) {
	switch Scenario(s) {
	case ScenarioNone, ScenarioFailPing, ScenarioSlowBoot,
		ScenarioConsoleDrop, ScenarioSSHDown, ScenarioTunnelDrop, ScenarioTunnelFail,
		ScenarioConsoleFail:
		return Scenario(s), nil
	}
	return "", fmt.Errorf("unknown STUB_SCENARIO %q (want fail-ping, slow-boot, console-drop, ssh-down, tunnel-drop, tunnel-fail, or console-fail)", s)
}

// VM state machine.
type state int

const (
	stShutOff state = iota
	stStarting
	stRunning
)

func (s state) String() string {
	switch s {
	case stStarting:
		return "starting"
	case stRunning:
		return "running"
	default:
		return "shut off"
	}
}

// VMSpec describes one VM in the fake table.
type VMSpec struct {
	Name      string
	VCPUs     int
	MemoryMiB int
	IP        string // address while running ("", if none)
	VNCPort   int    // 0 if not VNC-enabled
	SpiceAddr string // "host:port" while running ("" if none)
	Initial   string // "", "shut off" or "running"
}

// defaultVMs is the deterministic fake host's table.
var defaultVMs = []VMSpec{
	{Name: "debian-dev", VCPUs: 2, MemoryMiB: 4096, IP: "192.168.100.50", SpiceAddr: "127.0.0.1:5991"},
	{Name: "win10", VCPUs: 4, MemoryMiB: 8192, IP: "192.168.168.51", VNCPort: 5901},
	// The "console drip": its serial console emits one line every
	// dripInterval for a few seconds after the banner. Deterministic
	// incremental output for the reverse-proxy arrival specs
	// (e2e tests/proxy.spec.ts): a buffering proxy coalesces the
	// drip into one batch, an unbuffered one keeps the arrival
	// spread in time.
	{Name: "drip", VCPUs: 1, MemoryMiB: 1024},
}

type vm struct {
	spec  VMSpec
	state state
	timer *time.Timer // pending start → running
}

// Backend is a deterministic fake implementation of virt.Backend.
type Backend struct {
	srv       virt.Server
	bootDelay time.Duration
	scenario  Scenario
	mu        sync.Mutex
	vms       map[string]*vm
	order     []string // definition order (deterministic ListVMs)
}

// Option configures a Backend.
type Option func(*Backend)

// WithBootDelay sets the delay between start and "running".
func WithBootDelay(d time.Duration) Option {
	return func(b *Backend) { b.bootDelay = d }
}

// WithScenario sets an injectable failure scenario.
func WithScenario(s Scenario) Option {
	return func(b *Backend) { b.scenario = s }
}

// WithVMs replaces the default VM table.
func WithVMs(specs []VMSpec) Option {
	return func(b *Backend) {
		b.vms = map[string]*vm{}
		b.order = nil
		for _, spec := range specs {
			b.addVM(spec)
		}
	}
}

// New creates a stub Backend for the (fake) host srv.
func New(srv virt.Server, opts ...Option) *Backend {
	b := &Backend{
		srv:       srv,
		bootDelay: defaultBootDelay,
		vms:       map[string]*vm{},
	}
	for _, spec := range defaultVMs {
		b.addVM(spec)
	}
	for _, opt := range opts {
		opt(b)
	}
	if b.scenario == ScenarioSlowBoot && b.bootDelay < slowBootDelay {
		b.bootDelay = slowBootDelay
	}
	return b
}

// BootDelay exposes the effective start → running delay (tests).
func (b *Backend) BootDelay() time.Duration { return b.bootDelay }

// VMNames returns the VM table in definition order (tests).
func (b *Backend) VMNames() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.order))
	copy(out, b.order)
	return out
}

func (b *Backend) addVM(spec VMSpec) {
	st := stShutOff
	switch spec.Initial {
	case "running":
		st = stRunning
	case "shut off", "":
		st = stShutOff
	default:
		panic(fmt.Sprintf("stub: bad initial state %q for %q", spec.Initial, spec.Name))
	}
	b.vms[spec.Name] = &vm{spec: spec, state: st}
	b.order = append(b.order, spec.Name)
}

func (b *Backend) lookup(name string) (*vm, error) {
	vm, ok := b.vms[name] // caller holds b.mu
	if !ok {
		return nil, fmt.Errorf("domain '%s' not found", name)
	}
	return vm, nil
}

// dialError mimics an ssh failure to the (fake) host.
func (b *Backend) dialError() error {
	port := b.srv.Port
	if port == 0 {
		port = 22
	}
	return fmt.Errorf("ssh: connect to %s@%s:%d: Connection refused",
		b.srv.User, b.srv.Host, port)
}

// Ping implements virt.Backend.
func (b *Backend) Ping(ctx context.Context) error {
	if b.scenario == ScenarioFailPing || b.scenario == ScenarioSSHDown {
		return b.dialError()
	}
	return nil
}

// ListVMs implements virt.Backend.
func (b *Backend) ListVMs(ctx context.Context) ([]virt.VM, error) {
	if b.scenario == ScenarioSSHDown {
		return nil, b.dialError()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]virt.VM, 0, len(b.order))
	for _, name := range b.order {
		out = append(out, b.vmToVirt(b.vms[name]))
	}
	return out, nil
}

// VMInfo implements virt.Backend.
func (b *Backend) VMInfo(ctx context.Context, name string) (virt.VM, error) {
	if b.scenario == ScenarioSSHDown {
		return virt.VM{}, b.dialError()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	vm, err := b.lookup(name)
	if err != nil {
		return virt.VM{}, err
	}
	return b.vmToVirt(vm), nil
}

func (b *Backend) vmToVirt(vm *vm) virt.VM {
	v := virt.VM{
		Name:    vm.spec.Name,
		State:   vm.state.String(),
		Running: vm.state != stShutOff,
		VCPUs:   vm.spec.VCPUs,
		Memory:  vm.spec.MemoryMiB,
		VNCPort: vm.spec.VNCPort,
	}
	if vm.state == stRunning {
		v.IP = vm.spec.IP
		v.SpiceAddr = vm.spec.SpiceAddr
	}
	return v
}

// Define implements virt.Backend: registers a new shut-off domain
// parsed from the XML (only the <name> is used; the stub does not
// model resources).
func (b *Backend) Define(ctx context.Context, xml string) error {
	if b.scenario == ScenarioSSHDown {
		return b.dialError()
	}
	name := virt.DomainName(xml)
	if name == "" {
		return fmt.Errorf("no domain name in XML")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.vms[name]; ok {
		return fmt.Errorf("cannot define domain %q: with the same name", name)
	}
	b.addVM(VMSpec{Name: name, VCPUs: 2, MemoryMiB: 2048})
	return nil
}

// Undefine implements virt.Backend: removes the domain definition;
// refuses while the domain is running (destroy it first).
func (b *Backend) Undefine(ctx context.Context, name string) error {
	if b.scenario == ScenarioSSHDown {
		return b.dialError()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	vm, ok := b.vms[name]
	if !ok {
		return fmt.Errorf("domain %q not found", name)
	}
	if vm.state != stShutOff {
		return fmt.Errorf("cannot undefine domain %q: domain is running", name)
	}
	delete(b.vms, name)
	for i, n := range b.order {
		if n == name {
			b.order = append(b.order[:i], b.order[i+1:]...)
			break
		}
	}
	return nil
}

// DumpXML implements virt.Backend with a canned domain XML shaped like
// real `virsh dumpxml` output (the @real tier is the authority); the
// fields track the stub VM's spec so UI tests see plausible content.
func (b *Backend) DumpXML(ctx context.Context, name string) (string, error) {
	if b.scenario == ScenarioSSHDown {
		return "", b.dialError()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	vm, ok := b.vms[name]
	if !ok {
		return "", fmt.Errorf("domain %q not found", name)
	}
	s := vm.spec
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<domain type='kvm'>
  <name>%s</name>
  <uuid>%s</uuid>
  <memory unit='KiB'>%d</memory>
  <currentMemory unit='KiB'>%d</currentMemory>
  <vcpu placement='static'>%d</vcpu>
  <os>
    <type arch='x86_64' machine='pc'>hvm</type>
  </os>
  <devices>
    <emulator>/usr/bin/qemu-system-x86_64</emulator>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/libvirt/images/%s.qcow2'/>
    </disk>
    <interface type='network'>
      <mac address='%s'/>
      <source network='default' port='1'/>
      <model type='virtio'/>
    </interface>
%s  </devices>
</domain>
`,
		name, macFromName(name), s.MemoryMiB*1024, s.MemoryMiB*1024, s.VCPUs, name, macFromName(name),
		graphicsXML(s)), nil
}

// graphicsXML renders the display device the way dumpxml would: SPICE
// when the spec has an address, VNC when it has a VNC port.
func graphicsXML(s VMSpec) string {
	switch {
	case s.SpiceAddr != "":
		return "    <graphics type='spice' port='5900' autoport='no' listen='127.0.0.1'/>\n"
	case s.VNCPort > 0:
		return fmt.Sprintf("    <graphics type='vnc' port='%d' listen='127.0.0.1'/>\n", s.VNCPort)
	default:
		return ""
	}
}

// macFromName derives a deterministic OUI-prefixed MAC from a name
// (purely cosmetic; keeps the canned XML stable across calls).
func macFromName(name string) string {
	var h uint32 = 2166136261 // FNV-1a
	for i := 0; i < len(name); i++ {
		h ^= uint32(name[i])
		h *= 16777619
	}
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", h>>8&0xff, h>>16&0xff, h&0xff)
}

// Start implements virt.Backend. The transition to running is delayed
// by bootDelay (the observable in-between state is "starting").
func (b *Backend) Start(ctx context.Context, name string) error {
	if b.scenario == ScenarioSSHDown {
		return b.dialError()
	}
	b.mu.Lock()
	v, err := b.lookup(name)
	if err != nil {
		b.mu.Unlock()
		return err
	}
	switch v.state {
	case stRunning, stStarting:
		b.mu.Unlock()
		return fmt.Errorf("domain '%s' is already running", name)
	case stShutOff:
		v.state = stStarting
	}
	b.mu.Unlock()
	b.completeBoot(v)
	return nil
}

func (b *Backend) completeBoot(v *vm) {
	b.mu.Lock()
	if b.bootDelay <= 0 {
		if v.state == stStarting {
			v.state = stRunning
		}
		b.mu.Unlock()
		return
	}
	if v.state == stStarting {
		v.timer = time.AfterFunc(b.bootDelay, func() {
			b.mu.Lock()
			if v.state == stStarting {
				v.state = stRunning
			}
			b.mu.Unlock()
		})
	}
	b.mu.Unlock()
}

// Shutdown implements virt.Backend (graceful).
func (b *Backend) Shutdown(ctx context.Context, name string) error {
	if b.scenario == ScenarioSSHDown {
		return b.dialError()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	v, err := b.lookup(name)
	if err != nil {
		return err
	}
	if v.state != stRunning {
		return fmt.Errorf("domain '%s' is not running", name)
	}
	v.state = stShutOff
	return nil
}

// Destroy implements virt.Backend (force). Cancels a pending boot.
func (b *Backend) Destroy(ctx context.Context, name string) error {
	if b.scenario == ScenarioSSHDown {
		return b.dialError()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	v, err := b.lookup(name)
	if err != nil {
		return err
	}
	switch v.state {
	case stRunning:
		v.state = stShutOff
	case stStarting:
		v.state = stShutOff
		if v.timer != nil {
			v.timer.Stop()
		}
	case stShutOff:
		return fmt.Errorf("domain '%s' is not running", name)
	}
	return nil
}

// Reboot implements virt.Backend. The VM goes through "starting" again.
func (b *Backend) Reboot(ctx context.Context, name string) error {
	if b.scenario == ScenarioSSHDown {
		return b.dialError()
	}
	b.mu.Lock()
	v, err := b.lookup(name)
	if err != nil {
		b.mu.Unlock()
		return err
	}
	if v.state != stRunning {
		b.mu.Unlock()
		return fmt.Errorf("domain '%s' is not running", name)
	}
	v.state = stStarting
	b.mu.Unlock()
	b.completeBoot(v)
	return nil
}

// Console implements virt.Backend with a byte-deterministic fake shell:
// banner + motd, then a getty-style prompt that echoes input and
// answers a handful of canned commands. The caller must close in when
// it is done (the API layer does this via the session pipes).
func (b *Backend) Console(ctx context.Context, name string, in io.Reader, out io.Writer) error {
	if b.scenario == ScenarioSSHDown {
		return b.dialError()
	}
	if b.scenario == ScenarioConsoleFail {
		return fmt.Errorf("stub: console attach for %q failed (scenario console-fail)", name)
	}
	b.mu.Lock()
	v, err := b.lookup(name)
	if err != nil {
		b.mu.Unlock()
		return err
	}
	if v.state != stRunning {
		b.mu.Unlock()
		return fmt.Errorf("domain '%s' is not running", name)
	}
	b.mu.Unlock()

	p := profileFor(v.spec)
	_, _ = io.WriteString(out, p.banner)
	_, _ = io.WriteString(out, p.motd)

	if v.spec.Name == "drip" {
		if err := dripOutput(ctx, out); err != nil {
			return err // client went away mid-drip
		}
	}

	if b.scenario == ScenarioConsoleDrop {
		// Stream goes away shortly after the banner: the UI's
		// "stream ended / reconnect" path.
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(consoleDropDelay):
			return nil
		}
	}
	_, _ = io.WriteString(out, p.prompt)
	return b.shell(ctx, v, p, in, out)
}

// dripOutput emits one line every dripInterval, dripLines times —
// the deterministic incremental-output source for the proxy specs.
// It returns non-nil if the client disconnects mid-drip.
const (
	dripInterval = 300 * time.Millisecond
	dripLines    = 8
)

func dripOutput(ctx context.Context, out io.Writer) error {
	for i := 0; i < dripLines; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dripInterval):
		}
		if _, err := fmt.Fprintf(out, "tick %d\n", i); err != nil {
			return err
		}
	}
	return nil
}

// shellProfile holds the per-VM terminal strings.
type shellProfile struct {
	prompt     string
	banner     string
	motd       string
	lineEnding string
	unameReply string
	notFound   func(cmd string) string
}

func profileFor(spec VMSpec) shellProfile {
	if strings.HasPrefix(spec.Name, "win") {
		return shellProfile{
			prompt:     "C:\\>",
			banner:     "Microsoft Windows [Version 10.0.19045.3636]\r\n(c) Microsoft Corporation. All rights reserved.\r\n",
			lineEnding: "\r\n",
			unameReply: "Microsoft Windows [Version 10.0.19045.3636]",
			notFound: func(cmd string) string {
				return cmd + " is not recognized as an internal or external command,\r\noperable program or batch file."
			},
		}
	}
	return shellProfile{
		prompt:     spec.Name + ":~# ",
		banner:     "Linux " + spec.Name + " 6.12.0-stub #1 SMP PREEMPT_DYNAMIC x86_64 GNU/Linux\n",
		motd:       "Welcome to the virtweb stub console (offline demo).\n",
		lineEnding: "\n",
		unameReply: "Linux " + spec.Name + " 6.12.0-stub #1 SMP PREEMPT_DYNAMIC x86_64 GNU/Linux",
		notFound: func(cmd string) string {
			return "bash: " + cmd + ": command not found"
		},
	}
}

// shell echoes input like a cooked getty line by line and answers
// canned commands. It returns when in reaches EOF or ctx is cancelled.
func (b *Backend) shell(ctx context.Context, vm *vm, p shellProfile, in io.Reader, out io.Writer) error {
	one := make([]byte, 1)
	type inByte struct {
		b   byte
		err error
	}
	inCh := make(chan inByte)
	go func() {
		for {
			_, err := in.Read(one)
			if err != nil {
				inCh <- inByte{err: err}
				return
			}
			inCh <- inByte{b: one[0]}
		}
	}()
	var line []byte
	for {
		select {
		case <-ctx.Done():
			return nil
		case ib := <-inCh:
			if ib.err != nil {
				return nil // peer closed (client disconnected)
			}
			switch ib.b {
			case '\n', '\r':
				_, _ = io.WriteString(out, p.lineEnding) // echo the line ending
				cmd := strings.TrimSpace(string(line))
				line = line[:0]
				if reply := b.reply(vm, p, cmd); reply != "" {
					_, _ = io.WriteString(out, reply)
				}
				if cmd == "exit" || cmd == "logout" {
					return nil
				}
				_, _ = io.WriteString(out, p.prompt)
			default:
				_, _ = out.Write([]byte{ib.b}) // echo like a real getty
				line = append(line, ib.b)
			}
		}
	}
}

// Tunnel implements virt.Backend with a deterministic fake TCP service:
// a small banner written in spaced chunks (so stream consumers see
// incremental arrival, like a real tunneled service), then a raw echo
// of everything sent in, until the session ends.
func (b *Backend) Tunnel(ctx context.Context, name, target string, in io.Reader, out io.Writer) error {
	if b.scenario == ScenarioSSHDown {
		return b.dialError()
	}
	b.mu.Lock()
	_, err := b.lookup(name)
	b.mu.Unlock()
	if err != nil {
		return err
	}
	for _, line := range []string{
		"stub-tunnel: ", name, " ", target, "\n", "chunk 1\n", "chunk 2\n",
	} {
		if _, werr := io.WriteString(out, line); werr != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(30 * time.Millisecond):
		}
	}
	if b.scenario == ScenarioTunnelDrop {
		// Remote end hangs up shortly after the banner: the UI's
		// "stream ended" path.
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(consoleDropDelay):
			return nil
		}
	}
	if b.scenario == ScenarioTunnelFail {
		// The tunnel dies shortly after the banner with an error: the
		// 1011 close carries this message in the close reason.
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(consoleDropDelay):
			return fmt.Errorf("stub: tunnel to %s failed (scenario tunnel-fail)", target)
		}
	}
	buf := make([]byte, 4096)
	for {
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return nil
			}
		}
		if rerr != nil {
			return nil
		}
	}
}

// reply is the canned command table.
func (b *Backend) reply(vm *vm, p shellProfile, cmd string) string {
	switch cmd {
	case "":
		return ""
	case "uname -a":
		return p.unameReply + p.lineEnding
	case "hostname":
		return vm.spec.Name + p.lineEnding
	case "exit", "logout":
		return ""
	default:
		return p.notFound(cmd) + p.lineEnding
	}
}

// Factory implements the api.BackendFactory interface for the stub
// backend.
//
// A Factory returns the same Backend instance for a given server ID, so
// every consumer (the REST API and the web UI each cache their own
// backend per server) observes the same fake host state. Without this,
// a "start" via /api would update one in-memory stub while the console
// page read a different one.
type Factory struct {
	Scenario  Scenario
	VMs       []VMSpec      // optional table override
	BootDelay time.Duration // optional delay override

	mu       sync.Mutex
	byServer map[string]virt.Backend
}

// NewBackend implements api.BackendFactory.
func (f *Factory) NewBackend(srv virt.Server) virt.Backend {
	f.mu.Lock()
	if b, ok := f.byServer[srv.ID]; ok {
		f.mu.Unlock()
		return b
	}
	var opts []Option
	if f.Scenario != ScenarioNone {
		opts = append(opts, WithScenario(f.Scenario))
	}
	if len(f.VMs) > 0 {
		opts = append(opts, WithVMs(f.VMs))
	}
	if f.BootDelay > 0 {
		opts = append(opts, WithBootDelay(f.BootDelay))
	}
	b := New(srv, opts...)
	if f.byServer == nil {
		f.byServer = map[string]virt.Backend{}
	}
	f.byServer[srv.ID] = b
	f.mu.Unlock()
	return b
}
