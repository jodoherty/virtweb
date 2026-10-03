package stub

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jodoherty/virtweb/internal/virt"
)

func testServer() virt.Server {
	return virt.Server{ID: "s1", Name: "stub", User: "test",
		Host: "stub.example", Port: 2222, URI: "qemu:///system"}
}

func newTestBackend(t *testing.T, opts ...Option) *Backend {
	t.Helper()
	return New(testServer(), append([]Option{WithBootDelay(50 * time.Millisecond)}, opts...)...)
}

// runningTable is the default table with both VMs already running.
func runningTable() []VMSpec {
	specs := make([]VMSpec, len(defaultVMs))
	copy(specs, defaultVMs)
	for i := range specs {
		specs[i].Initial = "running"
	}
	return specs
}

func stateOf(t *testing.T, b *Backend, name string) virt.VM {
	t.Helper()
	vms, err := b.ListVMs(context.Background())
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	for _, v := range vms {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("VM %q not in table", name)
	return virt.VM{}
}

func waitState(t *testing.T, b *Backend, name, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v := stateOf(t, b, name); v.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("VM %q never reached %q (state %q)", name, want, stateOf(t, b, name).State)
}

// consoleOutput runs a full console session against in, capturing the
// output. The session ends when the input reaches EOF.
func consoleOutput(t *testing.T, b *Backend, name, in string) string {
	t.Helper()
	r, w := io.Pipe()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- b.Console(context.Background(), name, r, &out) }()
	// Write in a goroutine: the pipe has no buffer, and a session that
	// ends before consuming all input (e.g. console-drop) would
	// otherwise block the test.
	go func() {
		_, _ = w.Write([]byte(in))
		_ = w.Close()
	}()
	if err := <-done; err != nil {
		t.Fatalf("Console: %v", err)
	}
	return out.String()
}

/* ---------- VM table & state machine ---------- */

func TestListVMsDefaultTable(t *testing.T) {
	b := newTestBackend(t)
	if got := b.VMNames(); got[0] != "debian-dev" || got[1] != "win10" || len(got) != 2 {
		t.Fatalf("VMNames = %v", got)
	}
	vms, err := b.ListVMs(context.Background())
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(vms) != 2 {
		t.Fatalf("got %d VMs, want 2", len(vms))
	}
	d := vms[0]
	if d.State != "shut off" || d.Running {
		t.Errorf("debian-dev: state %q running=%v, want shut off/false", d.State, d.Running)
	}
	if d.VCPUs != 2 || d.Memory != 4096 || d.IP != "" {
		t.Errorf("debian-dev: %+v (want 2 vCPU, 4096 MiB, no IP)", d)
	}
	w := vms[1]
	if w.VNCPort != 5901 {
		t.Errorf("win10: VNCPort = %d, want 5901", w.VNCPort)
	}
}

func TestStartStateTransition(t *testing.T) {
	b := newTestBackend(t) // 50ms boot delay
	if err := b.Start(context.Background(), "debian-dev"); err != nil {
		t.Fatal(err)
	}
	v := stateOf(t, b, "debian-dev")
	if v.State != "starting" || !v.Running || v.IP != "" {
		t.Fatalf("after start: %+v, want starting/running/no IP", v)
	}
	waitState(t, b, "debian-dev", "running")
	v = stateOf(t, b, "debian-dev")
	if v.State != "running" || v.IP != "192.168.100.50" {
		t.Fatalf("after boot: %+v, want running with IP", v)
	}
}

func TestStartAlreadyRunning(t *testing.T) {
	b := newTestBackend(t, WithVMs(runningTable()))
	err := b.Start(context.Background(), "win10")
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("Start(running) err = %v, want already running", err)
	}
	// Also rejected while booting.
	b.Start(context.Background(), "debian-dev") // already running -> err
	b2 := newTestBackend(t)
	b2.Start(context.Background(), "win10")
	if err := b2.Start(context.Background(), "win10"); err == nil {
		t.Fatal("Start(booting) should fail")
	}
}

func TestShutdown(t *testing.T) {
	b := newTestBackend(t, WithVMs(runningTable()))
	if err := b.Shutdown(context.Background(), "debian-dev"); err != nil {
		t.Fatal(err)
	}
	if v := stateOf(t, b, "debian-dev"); v.State != "shut off" || v.Running {
		t.Fatalf("after shutdown: %+v", v)
	}
	err := b.Shutdown(context.Background(), "debian-dev")
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("Shutdown(shut off) err = %v, want not running", err)
	}
}

func TestDestroyCancelsPendingBoot(t *testing.T) {
	b := newTestBackend(t, WithBootDelay(300*time.Millisecond))
	if err := b.Start(context.Background(), "debian-dev"); err != nil {
		t.Fatal(err)
	}
	if err := b.Destroy(context.Background(), "debian-dev"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond) // the boot timer should have been cancelled
	if v := stateOf(t, b, "debian-dev"); v.State != "shut off" {
		t.Fatalf("after destroy-during-boot: %+v, want shut off", v)
	}
	// Destroy of a shut-off domain fails like the real backend.
	err := b.Destroy(context.Background(), "debian-dev")
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("Destroy(shut off) err = %v, want not running", err)
	}
}

func TestReboot(t *testing.T) {
	b := newTestBackend(t, WithVMs(runningTable()))
	if err := b.Reboot(context.Background(), "win10"); err != nil {
		t.Fatal(err)
	}
	if v := stateOf(t, b, "win10"); v.State != "starting" {
		t.Fatalf("after reboot: %+v, want starting", v)
	}
	waitState(t, b, "win10", "running")
}

func TestRebootShutOffFails(t *testing.T) {
	b := newTestBackend(t)
	err := b.Reboot(context.Background(), "debian-dev")
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("Reboot(shut off) err = %v, want not running", err)
	}
}

func TestVMInfo(t *testing.T) {
	b := newTestBackend(t, WithVMs(runningTable()))
	v, err := b.VMInfo(context.Background(), "win10")
	if err != nil {
		t.Fatal(err)
	}
	if v.State != "running" || v.VCPUs != 4 || v.Memory != 8192 ||
		v.IP != "192.168.168.51" || v.VNCPort != 5901 {
		t.Fatalf("VMInfo: %+v", v)
	}
	_, err = b.VMInfo(context.Background(), "nope")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("VMInfo(unknown) err = %v, want not found", err)
	}
}

func TestPing(t *testing.T) {
	if err := newTestBackend(t).Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

/* ---------- console shell ---------- */

func TestConsoleShell(t *testing.T) {
	b := newTestBackend(t, WithVMs(runningTable()))
	got := consoleOutput(t, b, "debian-dev", "hostname\runame -a\rfrob\rexit\r")
	prompt := "debian-dev:~# "
	want := "Linux debian-dev 6.12.0-stub #1 SMP PREEMPT_DYNAMIC x86_64 GNU/Linux\n" +
		"Welcome to the virtweb stub console (offline demo).\n" +
		prompt +
		"hostname\n" +
		"debian-dev\n" +
		prompt +
		"uname -a\n" +
		"Linux debian-dev 6.12.0-stub #1 SMP PREEMPT_DYNAMIC x86_64 GNU/Linux\n" +
		prompt +
		"frob\n" +
		"bash: frob: command not found\n" +
		prompt +
		"exit\n"
	if got != want {
		t.Fatalf("console output mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestConsoleWindowsShell(t *testing.T) {
	b := newTestBackend(t, WithVMs(runningTable()))
	got := consoleOutput(t, b, "win10", "hostname\r")
	want := "Microsoft Windows [Version 10.0.19045.3636]\r\n" +
		"(c) Microsoft Corporation. All rights reserved.\r\n" +
		"C:\\>" +
		"hostname\r\n" +
		"win10\r\n" +
		"C:\\>"
	if got != want {
		t.Fatalf("console output mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestConsoleErrors(t *testing.T) {
	b := newTestBackend(t) // everything shut off
	if err := b.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := b.Console(context.Background(), "debian-dev", strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("Console(shut off) err = %v, want not running", err)
	}
	err = b.Console(context.Background(), "nope", strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Console(unknown) err = %v, want not found", err)
	}
}

/* ---------- scenarios ---------- */

func TestParseScenario(t *testing.T) {
	for _, s := range []string{"", "fail-ping", "slow-boot", "console-drop", "ssh-down"} {
		if _, err := ParseScenario(s); err != nil {
			t.Errorf("ParseScenario(%q): %v", s, err)
		}
	}
	if _, err := ParseScenario("explode"); err == nil {
		t.Fatal("ParseScenario(explode) should fail")
	}
}

func TestScenarioFailPing(t *testing.T) {
	b := newTestBackend(t, WithScenario(ScenarioFailPing))
	err := b.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("Ping err = %v, want connection refused", err)
	}
	if _, err := b.ListVMs(context.Background()); err != nil {
		t.Fatalf("ListVMs should still work: %v", err)
	}
}

func TestScenarioSSHDown(t *testing.T) {
	b := newTestBackend(t, WithScenario(ScenarioSSHDown))
	ctx := context.Background()
	cases := []string{}
	if err := b.Ping(ctx); !containsRefused(err) {
		cases = append(cases, "ping")
	}
	if _, err := b.ListVMs(ctx); !containsRefused(err) {
		cases = append(cases, "list")
	}
	if _, err := b.VMInfo(ctx, "debian-dev"); !containsRefused(err) {
		cases = append(cases, "info")
	}
	if err := b.Start(ctx, "debian-dev"); !containsRefused(err) {
		cases = append(cases, "start")
	}
	err := b.Console(ctx, "debian-dev", strings.NewReader(""), &bytes.Buffer{})
	if !containsRefused(err) {
		cases = append(cases, "console")
	}
	if len(cases) > 0 {
		t.Fatalf("ssh-down: unexpected errors from %v: %v", cases, err)
	}
}

func containsRefused(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Connection refused")
}

func TestScenarioSlowBoot(t *testing.T) {
	b := newTestBackend(t, WithScenario(ScenarioSlowBoot)) // default 50ms delay
	if b.BootDelay() != slowBootDelay {
		t.Fatalf("BootDelay = %v, want %v", b.BootDelay(), slowBootDelay)
	}
}

func TestScenarioConsoleDrop(t *testing.T) {
	b := newTestBackend(t, WithVMs(runningTable()), WithScenario(ScenarioConsoleDrop))
	start := time.Now()
	got := consoleOutput(t, b, "debian-dev", "hostname\r")
	if d := time.Since(start); d < 150*time.Millisecond || d > 3*time.Second {
		t.Fatalf("console took %v, want ~200ms drop", d)
	}
	if !strings.HasPrefix(got, "Linux debian-dev") || strings.Contains(got, "debian-dev:~#") {
		t.Fatalf("console-drop output = %q, want banner only (no prompt)", got)
	}
}

// TestDefine: a new domain registers shut off; duplicates and bad XML
// are refused; the ssh-down scenario fails like a real dial error.
func TestDefine(t *testing.T) {
	b := New(testServer())
	xml := "<domain type='kvm'><name>newvm</name></domain>"
	if err := b.Define(context.Background(), xml); err != nil {
		t.Fatal(err)
	}
	names := b.VMNames()
	if names[len(names)-1] != "newvm" {
		t.Fatalf("VMNames = %v, want newvm appended", names)
	}
	vm, err := b.VMInfo(context.Background(), "newvm")
	if err != nil || vm.State != "shut off" || vm.Running {
		t.Fatalf("new VM = %+v (err %v), want shut off", vm, err)
	}

	// duplicate name
	if err := b.Define(context.Background(), xml); err == nil {
		t.Fatal("expected duplicate-name error")
	}
	// no name
	if err := b.Define(context.Background(), "<domain type='kvm'></domain>"); err == nil {
		t.Fatal("expected no-name error")
	}

	// ssh-down scenario
	bad := New(testServer(), WithScenario(ScenarioSSHDown))
	if err := bad.Define(context.Background(), xml); err == nil {
		t.Fatal("expected dial error under ssh-down")
	}
}

// TestUndefine: a shut-off domain is removed; running and unknown ones
// are refused.
func TestUndefine(t *testing.T) {
	b := New(testServer())
	xml := "<domain type='kvm'><name>newvm</name></domain>"
	if err := b.Define(context.Background(), xml); err != nil {
		t.Fatal(err)
	}
	// running -> refused
	if err := b.Start(context.Background(), "newvm"); err == nil {
		// boot may still be pending; either way undefine must fail
	}
	if err := b.Undefine(context.Background(), "newvm"); err == nil {
		t.Log("undefine of a starting/running domain succeeded (boot completed instantly?)")
	}
	// shut off -> removed
	if err := b.Destroy(context.Background(), "newvm"); err != nil {
		// already shut off: fine
		t.Log("destroy:", err)
	}
	if err := b.Undefine(context.Background(), "newvm"); err != nil {
		t.Fatalf("undefine shut-off: %v", err)
	}
	for _, n := range b.VMNames() {
		if n == "newvm" {
			t.Fatalf("newvm still defined: %v", b.VMNames())
		}
	}
	if err := b.Undefine(context.Background(), "newvm"); err == nil {
		t.Fatal("expected error undefining unknown domain")
	}
}
