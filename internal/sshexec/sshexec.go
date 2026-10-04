// Package sshexec runs commands on remote hosts using the system ssh
// client. The Runner interface is the only SSH surface the rest of the
// project sees, so it can be shimmed in tests.
package sshexec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Runner executes commands on a remote host over SSH.
type Runner interface {
	// Run executes command on the remote host and returns its stdout.
	// A non-zero exit status is an error whose message includes stderr.
	Run(ctx context.Context, user, host string, port int, command string) (string, error)

	// Connect starts command on the remote host with local streams attached
	// to its stdin/stdout/stderr, blocking until the remote command exits.
	// If tty is true a remote pty is requested (ssh -tt).
	Connect(ctx context.Context, user, host string, port int, tty bool, command string, in io.Reader, out io.Writer) error

	// Tunnel opens a local stdio TCP tunnel to target ("host:port") on the
	// remote side, blocking until ctx is done or the tunnel ends. in/out
	// are wired straight to the tunnel's byte streams (ssh -N -W).
	Tunnel(ctx context.Context, user, host string, port int, target string, in io.Reader, out io.Writer) error
}

// Std is the default Runner: it execs the local system ssh binary.
//
// sshPath defaults to "ssh" when empty.
type Std struct {
	SSHPath string
	// ExtraOpts are additional -o options, e.g. for testing.
	ExtraOpts []string
	// StrictHostKeyChecking is the value for -o StrictHostKeyChecking=;
	// "accept-new" when empty (trust first contact, verify thereafter).
	StrictHostKeyChecking string
	// Key, when set, is the SSH private key material (PEM). It is
	// written to a 0600 temp file per call and passed to ssh as -i —
	// and via IdentitiesOnly=yes excludes the ssh agent and the ~/.ssh
	// identity defaults, so only this key is offered. The temp file is
	// removed after the call.
	Key string
	// Password, when set, is offered for SSH password auth by wrapping
	// ssh in sshpass (-f <tempfile>, mode 0600, removed after the call,
	// so it never appears in a process list). BatchMode is dropped for
	// such calls so the password prompt can be answered; the sshpass
	// binary must be available (sshPassPath, default "sshpass").
	Password string
	// sshPassPath defaults to "sshpass" when empty (exported form kept
	// unexported on purpose: it is a test seam, set via
	// WithSSHPassPath).
	sshPassPath string
}

// WithSSHPassPath returns s with the sshpass binary path overridden
// (test seam).
func (s *Std) WithSSHPassPath(p string) *Std {
	s.sshPassPath = p
	return s
}

// baseOpts are the options used for every connection.
//
// BatchMode=yes disables interactive password/prompt auth (we assume
// key-based auth); StrictHostKeyChecking defaults to accept-new
// (OpenSSH >= 7.x), which automatically accepts new host keys but still
// verifies known ones. Operators on untrusted paths to a host can pick
// a stricter value (e.g. "no" with a seeded known_hosts); see the
// -host-key-checking flag.
func (s *Std) baseOpts(port int) []string {
	hostKey := s.StrictHostKeyChecking
	if hostKey == "" {
		hostKey = "accept-new"
	}
	opts := []string{
		"StrictHostKeyChecking=" + hostKey,
		"ConnectTimeout=10",
	}
	// An explicit key is offered alone (no agent, no ~/.ssh defaults,
	// see Std.Key); a password needs the interactive prompt BatchMode
	// would kill, so passwordless calls stay non-interactive.
	if s.Key != "" {
		opts = append(opts, "IdentitiesOnly=yes")
	}
	if s.Password == "" {
		opts = append(opts, "BatchMode=yes")
	}
	opts = append(opts, s.ExtraOpts...)
	args := []string{}
	for _, o := range opts {
		args = append(args, "-o", o)
	}
	if port > 0 && port != 22 {
		args = append(args, "-p", strconv.Itoa(port))
	}
	return args
}

func (s *Std) dest(user, host string) string {
	if user != "" {
		return user + "@" + host
	}
	return host
}

func (s *Std) sshPath() string {
	if s.SSHPath != "" {
		return s.SSHPath
	}
	return "ssh"
}

// prepare returns the exec.Cmd for the ssh call. Credentials never
// appear in argv: with a Password set, ssh is wrapped in sshpass fed
// from a 0600 temp file; with a Key set, the key material is written
// to a 0600 temp file passed via -i. The returned cleanup removes the
// temp file(s).
func (s *Std) prepare(ctx context.Context, args []string) (*exec.Cmd, func(), error) {
	remove := []func(){}
	cleanup := func() {
		for _, f := range remove {
			f()
		}
	}

	if s.Key != "" {
		key, err := os.CreateTemp("", "virtweb-sshkey-*")
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
		remove = append(remove, func() { os.Remove(key.Name()) })
		if _, err := key.WriteString(s.Key); err != nil {
			key.Close()
			cleanup()
			return nil, func() {}, err
		}
		if err := key.Close(); err != nil {
			cleanup()
			return nil, func() {}, err
		}
		args = append([]string{"-i", key.Name()}, args...)
	}

	if s.Password == "" {
		return exec.CommandContext(ctx, s.sshPath(), args...), cleanup, nil
	}
	f, err := os.CreateTemp("", "virtweb-sshpass-*")
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	remove = append(remove, func() { os.Remove(f.Name()) })
	if _, err := f.WriteString(s.Password); err != nil {
		f.Close()
		cleanup()
		return nil, func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	passPath := s.sshPassPath
	if passPath == "" {
		passPath = "sshpass"
	}
	return exec.CommandContext(ctx, passPath,
		append([]string{"-f", f.Name(), s.sshPath()}, args...)...), cleanup, nil
}

// Run implements Runner.Run.
func (s *Std) Run(ctx context.Context, user, host string, port int, command string) (string, error) {
	args := append(s.baseOpts(port), s.dest(user, host), command)
	cmd, cleanup, err := s.prepare(ctx, args)
	if err != nil {
		return "", err
	}
	defer cleanup()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := filterStderr(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("ssh command failed: %s", trimTrailing(msg))
	}
	return stdout.String(), nil
}

// Connect implements Runner.Connect.
func (s *Std) Connect(ctx context.Context, user, host string, port int, tty bool, command string, in io.Reader, out io.Writer) error {
	args := s.baseOpts(port)
	if tty {
		args = append(args, "-tt")
	} else {
		args = append(args, "-T")
	}
	args = append(args, s.dest(user, host), command)
	cmd, cleanup, err := s.prepare(ctx, args)
	if err != nil {
		return err
	}
	defer cleanup()
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

// Tunnel implements Runner.Tunnel with `ssh -N -W target`. stderr is kept
// out of the data stream (it would corrupt binary protocols) and is only
// reported on failure.
func (s *Std) Tunnel(ctx context.Context, user, host string, port int, target string, in io.Reader, out io.Writer) error {
	args := s.baseOpts(port)
	args = append(args, "-N", "-W", target, s.dest(user, host))
	cmd, cleanup, err := s.prepare(ctx, args)
	if err != nil {
		return err
	}
	defer cleanup()
	cmd.Stdin = in
	cmd.Stdout = out
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		if msg := trimTrailing(stderr.String()); msg != "" {
			return fmt.Errorf("ssh tunnel %s: %v: %s", target, err, msg)
		}
	}
	return err
}
func filterStderr(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "setlocale:") {
			lines[i] = ""
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func trimTrailing(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
