package virsh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/jodoherty/virtweb/internal/virt"
)

type fakeRunner struct {
	mu    sync.Mutex
	commands []string
	// respond returns canned stdout for a command (may be nil); if fn is set
	// it is used instead.
	responses map[string]string
	fail      map[string]error
	fn        func(cmd string) (string, error)
}

func (f *fakeRunner) Run(_ context.Context, _, _ string, _ int, command string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	if f.fn != nil {
		return f.fn(command)
	}
	if err, ok := f.fail[command]; ok {
		return "", err
	}
	if out, ok := f.responses[command]; ok {
		return out, nil
	}
	return "", fmt.Errorf("virsh: command not understood: %s", command)
}

func (f *fakeRunner) Connect(_ context.Context, _, _ string, _ int, _ bool, command string, in io.Reader, out io.Writer) error {
	f.mu.Lock()
	f.commands = append(f.commands, command)
	f.mu.Unlock()
	// emulate an echo console: copy stdin to out, then write a banner.
	io.Copy(out, in)
	_, _ = out.Write([]byte("\nCONSOLE-CLOSED\n"))
	return nil
}

func (f *fakeRunner) Tunnel(_ context.Context, _, _ string, _ int, target string, in io.Reader, out io.Writer) error {
	f.mu.Lock()
	f.commands = append(f.commands, "tunnel "+target)
	f.mu.Unlock()
	// emulate an echo tunnel: copy stdin to out until stdin closes.
	io.Copy(out, in)
	return nil
}

func (f *fakeRunner) allCommands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

const listAll = ` Id   Name                    State
----------------------------------------------------
 *    fedora42                running
 -    win11                   shut off
 2    centos10                paused`

const domInfoFedora = ` Id:           4
 Name:         fedora42
 UUID:         abcd-1234
 CPU(s):       2
 CPU time:     12.3s
 Maximum memory: 4194304 KiB
 Used memory:    4194304 KiB
 State:         running
`

const domInfoWin11 = ` Id:           -
 Name:         win11
 UUID:         dead-beef
 CPU(s):       4
 CPU time:     0.0s
 Max memory: 2097152 KiB
 Used memory:  262144 KiB
 State:         shut off
 VNC:         192.168.1.10:5910 (id 1)
`

const domIfAddr = ` Name       MAC address          Protocol   Address
vnet0      52:54:00:aa:bb:cc     ipv4       192.168.122.15/24
`

func newFake() (*fakeRunner, *Backend) {
	fr := &fakeRunner{
		responses: map[string]string{
			`virsh -c qemu:///system list --all`:         listAll,
			`virsh -c qemu:///system start fedora42`:    "",
			`virsh -c qemu:///system shutdown fedora42`: "",
			`virsh -c qemu:///system destroy fedora42`:  "",
			`virsh -c qemu:///system reboot fedora42`:   "",
			`virsh -c qemu:///system dominfo fedora42`: domInfoFedora,
			`virsh -c qemu:///system dominfo win11`: domInfoWin11,
			`virsh -c qemu:///system domifaddr fedora42`: domIfAddr,
		},
		fail: map[string]error{
			`virsh -c qemu:///system domifaddr win11`:  errNoAddr,
			`virsh -c qemu:///system domifaddr centos10`: errNoAddr,
			`virsh -c qemu:///system dominfo centos10`:  errNoAddr,
		},
	}
	b := New(virt.Server{Name: "test", User: "u", Host: "h", URI: "qemu:///system"}, fr)
	return fr, b
}

var errNoAddr = errors.New("no ip address")

func TestListVMs(t *testing.T) {
	_, b := newFake()
	vms, err := b.ListVMs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 3 {
		t.Fatalf("got %d vms: %+v", len(vms), vms)
	}
	byName := map[string]virt.VM{}
	for _, vm := range vms {
		byName[vm.Name] = vm
	}
	fed := byName["fedora42"]
	if !fed.Running || fed.State != "running" || fed.VCPUs != 2 || fed.Memory != 4096 || fed.IP != "192.168.122.15" {
		t.Fatalf("fedora42 = %+v", fed)
	}
	win := byName["win11"]
	if win.Running || win.State != "shut off" || win.Memory != 2048 || win.VNCPort != 5910 {
		t.Fatalf("win11 = %+v", win)
	}
	// centos10 has no IP responses; it should still appear with its state
	c10 := byName["centos10"]
	if c10.State != "paused" {
		t.Fatalf("centos10 = %+v", c10)
	}
}

func TestActions(t *testing.T) {
	fr, b := newFake()
	ctx := context.Background()
	actions := []struct {
		fn   func(context.Context, string) error
		want string
	}{
		{b.Start, "start"},
		{b.Shutdown, "shutdown"},
		{b.Destroy, "destroy"},
		{b.Reboot, "reboot"},
	}
	for _, tc := range actions {
		if err := tc.fn(ctx, "fedora42"); err != nil {
			t.Fatalf("%s: %v", tc.want, err)
		}
	}
	cmds := fr.allCommands()
	if len(cmds) != 4 {
		t.Fatalf("commands = %v", cmds)
	}
	for _, tc := range actions {
		want := "virsh -c qemu:///system " + tc.want + " fedora42"
		found := false
		for _, c := range cmds {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Errorf("expected %q command, got %v", want, cmds)
		}
	}
}

func TestPing(t *testing.T) {
	fr, b := newFake()
	fr.responses[`virsh -c qemu:///system version`] = "virsh 10.0\n"
	if err := b.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConsole(t *testing.T) {
	_, b := newFake()
	buf := &strings.Builder{}
	err := b.Console(context.Background(), "fedora42", strings.NewReader("abc"), buf)
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "abc") || !strings.Contains(out, "CONSOLE-CLOSED") {
		t.Fatalf("console output = %q", out)
	}
}

func TestQuotedURI(t *testing.T) {
	// The libvirt URI is shell-quoted on the wire; verify the remote command
	// is still a clean virsh invocation.
	fr := &fakeRunner{responses: map[string]string{}}
	b := New(virt.Server{User: "u", Host: "h", URI: "qemu:///system"}, fr)
	fr.responses["virsh -c qemu:///system list --all"] = ""
	if _, err := b.ListVMs(context.Background()); err != nil {
		t.Fatal(err)
	}
	cmds := fr.allCommands()
	if len(cmds) != 1 || cmds[0] != "virsh -c qemu:///system list --all" {
		t.Fatalf("commands = %v", cmds)
	}
}

func TestParseListAll(t *testing.T) {
	names, states := parseListAll(listAll)
	if len(names) != 3 || names[0] != "fedora42" || names[1] != "win11" {
		t.Fatalf("names = %v", names)
	}
	if states[1] != "shut off" {
		t.Fatalf("multi-word state mangled: %v", states)
	}
}

func TestParseDomDisplay(t *testing.T) {
	cases := []struct {
		in         string
		proto, addr string
	}{
		{"spice://127.0.0.1:5900\n", "spice", "127.0.0.1:5900"},
		{"vnc://192.168.0.10:5900 (id 0)", "vnc", "192.168.0.10:5900"},
		{"spice://0.0.0.0:5991", "spice", "0.0.0.0:5991"},
		{"\n", "", ""},
		{"nonsense", "", ""},
	}
	for _, c := range cases {
		proto, addr := parseDomDisplay(c.in)
		if proto != c.proto || addr != c.addr {
			t.Errorf("parseDomDisplay(%q) = (%q,%q), want (%q,%q)", c.in, proto, addr, c.proto, c.addr)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":             "plain",
		"qemu:///system":    "qemu:///system", // : and / are shell-safe
		"it's":              "'it'\\''s'",
		"$(danger)":         "'$(danger)'",
		"semi;colon space":  "'semi;colon space'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
