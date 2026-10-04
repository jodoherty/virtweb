package sshexec

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSSH writes a script that records its arguments, echoes an echo arg,
// fails with stderr when the command contains "boom", and passes stdin
// through when the command contains "cat".
func fakeSSH(t *testing.T, argsFile string) string {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-ssh")
	os.WriteFile(script, []byte(`#!/bin/sh
printf '%s\n' "$@" > "` + argsFile + `"
args=""
for a in "$@"; do
  case "$a" in
    *boom*) echo "remote failure detail" >&2; exit 1;;
  esac
  args="$args $a"
done
echo "echo:$args"
case "$args" in
  *stdin-echo*) cat;;
esac
`), 0o755)
	return script
}

func TestStdRun(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	ssh := &Std{SSHPath: fakeSSH(t, argsFile)}

	out, err := ssh.Run(context.Background(), "bob", "example.com", 2222, "echo hi")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "-p 2222") || !strings.Contains(out, "bob@example.com") {
		t.Fatalf("args missing port/user: %q", out)
	}
	if !strings.Contains(out, "echo hi") {
		t.Fatalf("command missing: %q", out)
	}

	// default port must not add -p
	out, err = ssh.Run(context.Background(), "bob", "example.com", 0, "true")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "-p ") {
		t.Fatalf("unexpected -p for default port: %q", out)
	}

	// errors surface stderr
	if _, err := ssh.Run(context.Background(), "bob", "example.com", 0, "boom now"); err == nil {
		t.Fatal("expected error")
	} else if !strings.Contains(err.Error(), "remote failure detail") {
		t.Fatalf("error should contain stderr: %v", err)
	}
}

func TestStdConnect(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	ssh := &Std{SSHPath: fakeSSH(t, argsFile)}

	var out bytes.Buffer
	in := strings.NewReader("hello-from-local\n")
	if err := ssh.Connect(context.Background(), "bob", "h", 0, true, "stdin-echo cmd", in, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "hello-from-local") {
		t.Fatalf("stdin was not piped through: %q", out.String())
	}
	if !strings.Contains(out.String(), "-tt") {
		t.Fatalf("tty flag missing: %q", out.String())
	}

	// non-tty uses -T
	out.Reset()
	if err := ssh.Connect(context.Background(), "bob", "h", 0, false, "true", nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), " -T ") {
		t.Fatalf("-T flag missing: %q", out.String())
	}
}

func TestStdRunCancellation(t *testing.T) {
	// a real ssh against an unroutable address would hang; BatchMode plus
	// a cancelled context must stop us. Use the fake that blocks on a sleep.
	dir := t.TempDir()
	script := filepath.Join(dir, "block-ssh")
	os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755)
	ssh := &Std{SSHPath: script}
	ctx, cancel := context.WithTimeout(context.Background(), 500_000_000) // 0.5s
	defer cancel()
	if err := ssh.Connect(ctx, "u", "h", 0, false, "sleep", nil, bytes.NewBuffer(nil)); err == nil {
		t.Fatal("expected context deadline error")
	}
}

func TestStdTunnel(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	ssh := &Std{SSHPath: fakeSSH(t, argsFile)}

	var out bytes.Buffer
	in := strings.NewReader("tunnel-bytes\n")
	if err := ssh.Tunnel(context.Background(), "bob", "h", 0, "stdin-echo.h:5991", in, &out); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-N", "-W", "stdin-echo.h:5991", "bob@h"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("tunnel args missing %q: %q", want, raw)
		}
	}
	if !strings.Contains(out.String(), "tunnel-bytes") {
		t.Fatalf("stdin was not piped through the tunnel: %q", out.String())
	}

	// Failures carry stderr without mixing it into the data stream.
	if err := ssh.Tunnel(context.Background(), "bob", "h", 0, "boom:1", strings.NewReader(""), &out); err == nil {
		t.Fatal("expected error")
	} else if !strings.Contains(err.Error(), "remote failure detail") {
		t.Fatalf("error should contain stderr: %v", err)
	}
	if strings.Contains(out.String(), "remote failure detail") {
		t.Fatalf("stderr leaked into the data stream: %q", out.String())
	}
}

func TestFilterStderr(t *testing.T) {
	got := filterStderr("setlocale: No such file or directory\nerror: boom")
	want := "error: boom"
	if got != want {
		t.Fatalf("filterStderr = %q, want %q", got, want)
	}
	if got := filterStderr("setlocale: No such file or directory"); got != "" {
		t.Fatalf("all-noise stderr = %q, want empty", got)
	}
}

// TestStdHostKeyChecking: the StrictHostKeyChecking option defaults to
// accept-new and is operator-overridable (TOFU opt-out).
func TestStdHostKeyChecking(t *testing.T) {
	runWith := func(sk string) string {
		t.Helper()
		script := fakeSSH(t, filepath.Join(t.TempDir(), "args"))
		ssh := &Std{SSHPath: script, StrictHostKeyChecking: sk}
		out, err := ssh.Run(context.Background(), "bob", "example.com", 0, "true")
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// default: accept-new
	if out := runWith(""); !strings.Contains(out, "StrictHostKeyChecking=accept-new") {
		t.Fatalf("default args missing accept-new: %q", out)
	}
	// operator override
	if out := runWith("no"); !strings.Contains(out, "StrictHostKeyChecking=no") {
		t.Fatalf("override not passed through: %q", out)
	}
}

// fakeSSHPass records its own argv (the password must not appear in
// it), reads the -f file, records the password there, and execs the
// real (fake) ssh.
func fakeSSHPass(t *testing.T, argsFile, passFile, sshPath string) string {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-sshpass")
	os.WriteFile(script, []byte(`#!/bin/sh
printf '%s\n' "$@" > "` + argsFile + `"
while [ "$1" != "` + sshPath + `" ]; do
  if [ "$1" = "-f" ]; then
    cat "$2" > "` + passFile + `"
    shift
  fi
  shift
done
exec "` + sshPath + `" "$@"
`), 0o755)
	return script
}

func TestStdIdentityKey(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	ssh := &Std{SSHPath: fakeSSH(t, argsFile), Identity: "/tmp/keys/host.pem"}

	out, err := ssh.Run(context.Background(), "bob", "example.com", 0, "echo hi")
	if err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	a := string(args)
	for _, want := range []string{"-i", "/tmp/keys/host.pem", "-o", "IdentitiesOnly=yes", "BatchMode=yes"} {
		if !strings.Contains(a, want) {
			t.Fatalf("ssh args missing %q:\n%s", want, a)
		}
	}
	if !strings.Contains(out, "echo hi") {
		t.Fatalf("command missing: %q", out)
	}
}

func TestStdIdentityDisablesAgent(t *testing.T) {
	// IdentitiesOnly=yes must be passed as an -o option so the agent is
	// never consulted, and BatchMode stays on for key-only auth.
	argsFile := filepath.Join(t.TempDir(), "args")
	ssh := &Std{SSHPath: fakeSSH(t, argsFile), Identity: "/tmp/k"}
	if _, err := ssh.Run(context.Background(), "bob", "h", 0, "true"); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile)
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	// the -o IdentitiesOnly=yes pair must be present
	found := false
	for i, l := range lines {
		if l == "IdentitiesOnly=yes" && i > 0 && lines[i-1] == "-o" {
			found = true
		}
	}
	if !found {
		t.Fatalf("IdentitiesOnly=yes option missing:\n%s", string(args))
	}
}

func TestStdPasswordAuth(t *testing.T) {
	dir := t.TempDir()
	sshArgsFile := filepath.Join(dir, "ssh-args")
	passArgsFile := filepath.Join(dir, "pass-args")
	passFile := filepath.Join(dir, "pw")
	// fakeSSHPass hardcodes the ssh path in its exec line, so the fake
	// ssh must exist first.
	sshBin := fakeSSH(t, sshArgsFile)
	sp := fakeSSHPass(t, passArgsFile, passFile, sshBin)
	ssh := &Std{SSHPath: sshBin, Password: "hunter2-s3cret"}
	ssh.WithSSHPassPath(sp)

	out, err := ssh.Run(context.Background(), "bob", "example.com", 0, "echo hi")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "echo hi") {
		t.Fatalf("command missing: %q", out)
	}
	// the password reaches the ssh layer via the file, not argv
	pw, err := os.ReadFile(passFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(pw) != "hunter2-s3cret" {
		t.Fatalf("password file = %q", pw)
	}
	passArgs, _ := os.ReadFile(passArgsFile)
	if strings.Contains(string(passArgs), "hunter2-s3cret") {
		t.Fatalf("password leaked into sshpass argv:\n%s", passArgs)
	}
	// BatchMode must be off for password auth
	sshArgs, _ := os.ReadFile(sshArgsFile)
	if strings.Contains(string(sshArgs), "BatchMode=yes") {
		t.Fatalf("BatchMode still set for password auth:\n%s", sshArgs)
	}
}
