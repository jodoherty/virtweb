// Package sshexec runs commands on remote hosts using the system ssh
// client. The Runner interface is the only SSH surface the rest of the
// project sees, so it can be shimmed in tests.
package sshexec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
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
}

// baseOpts are the options used for every connection.
//
// BatchMode=yes disables interactive password/prompt auth (we assume
// key-based auth); StrictHostKeyChecking=accept-new (OpenSSH >= 7.x)
// automatically accepts new host keys but still verifies known ones.
func (s *Std) baseOpts(port int) []string {
	opts := []string{
		"BatchMode=yes",
		"StrictHostKeyChecking=accept-new",
		"ConnectTimeout=10",
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

// Run implements Runner.Run.
func (s *Std) Run(ctx context.Context, user, host string, port int, command string) (string, error) {
	args := append(s.baseOpts(port), s.dest(user, host), command)
	cmd := exec.CommandContext(ctx, s.sshPath(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
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
	cmd := exec.CommandContext(ctx, s.sshPath(), args...)
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

// Tunnel implements Runner.Tunnel with `ssh -N -W target`. stderr is kept
// out of the data stream (it would corrupt binary protocols) and is only
// reported on failure.
func (s *Std) Tunnel(ctx context.Context, user, host string, port int, target string, in io.Reader, out io.Writer) error {
	args := append(s.baseOpts(port), "-N", "-W", target, s.dest(user, host))
	cmd := exec.CommandContext(ctx, s.sshPath(), args...)
	cmd.Stdin = in
	cmd.Stdout = out
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if msg := trimTrailing(stderr.String()); msg != "" {
			return fmt.Errorf("ssh tunnel %s: %v: %s", target, err, msg)
		}
	}
	return err
}

func trimTrailing(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
