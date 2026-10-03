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
