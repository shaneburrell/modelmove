package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/shaneburrell/modelmove/internal/chunk"
	"github.com/shaneburrell/modelmove/internal/manifest"
	"github.com/shaneburrell/modelmove/internal/protocol"
	"github.com/shaneburrell/modelmove/internal/receiver"
)

// DefaultSSHCommand is the ssh client used when none is configured.
const DefaultSSHCommand = "ssh"

// DefaultRemoteBinary is the helper looked up on the remote PATH.
const DefaultRemoteBinary = "modelmove"

// sshSession runs a remote helper under ssh and speaks the framed protocol
// to it over the subprocess stdio.
type sshSession struct {
	cmd    *exec.Cmd
	client *protocol.Client
	stderr *stderrTail
	once   sync.Once
	closed error
	cfg    Config
}

func newSSHSession(ctx context.Context, e Endpoint, cfg Config) (Session, error) {
	sshCmd := cfg.SSHCommand
	if sshCmd == "" {
		sshCmd = DefaultSSHCommand
	}
	remoteBin := cfg.RemoteBin
	if remoteBin == "" {
		remoteBin = DefaultRemoteBinary
	}

	args := strings.Fields(sshCmd)
	if len(args) == 0 {
		return nil, errors.New("transport: empty ssh command")
	}
	prog, args := args[0], args[1:]
	if e.Port != 0 {
		args = append(args, "-p", strconv.Itoa(e.Port))
	}
	args = append(args, cfg.SSHOptions...)
	target := e.Host
	if e.User != "" {
		target = e.User + "@" + e.Host
	}
	args = append(args, target, remoteHelperCommand(remoteBin, e.Path, cfg))

	cmd := exec.CommandContext(ctx, prog, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	tail := &stderrTail{}
	cmd.Stderr = tail

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("transport: starting %s: %w", prog, err)
	}

	s := &sshSession{cmd: cmd, stderr: tail, cfg: cfg}
	client, err := protocol.NewClient(stdout, stdin, &pipeCloser{stdin: stdin, cmd: cmd}, protocol.ClientOptions{
		Tool: cfg.Tool,
		Root: e.Path,
		Warn: cfg.Warn,
	})
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, s.decorate(err)
	}
	s.client = client
	return s, nil
}

// remoteHelperCommand builds the shell command run on the far side. The path
// is single-quoted because it goes through the remote login shell.
func remoteHelperCommand(bin, path string, cfg Config) string {
	var b strings.Builder
	b.WriteString(shellQuote(bin))
	b.WriteString(" remote-helper --root ")
	b.WriteString(shellQuote(path))
	if cfg.Delete {
		b.WriteString(" --allow-delete")
	}
	return b.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (s *sshSession) Plan(ctx context.Context, m *manifest.Manifest) (*receiver.Plan, error) {
	plan, err := s.client.Plan(ctx, m, protocol.PlanRequest{
		Atomic:        string(s.cfg.Atomic),
		Dedupe:        s.cfg.Dedupe,
		Fast:          s.cfg.Fast,
		Resume:        s.cfg.Resume,
		Delete:        s.cfg.Delete,
		Verify:        s.cfg.Verify,
		PreserveTimes: s.cfg.PreserveTimes,
		Jobs:          s.cfg.Jobs,
	})
	return plan, s.decorate(err)
}

func (s *sshSession) BeginFile(ctx context.Context, rel string) error {
	return s.decorate(s.client.BeginFile(ctx, rel))
}

func (s *sshSession) SendChunk(ctx context.Context, d chunk.Digest, data []byte) error {
	return s.decorate(s.client.SendChunk(ctx, d, data))
}

func (s *sshSession) EndFile(ctx context.Context) (*receiver.FileResult, error) {
	res, err := s.client.EndFile(ctx)
	return res, s.decorate(err)
}

func (s *sshSession) Finish(ctx context.Context) (*receiver.Summary, error) {
	sum, err := s.client.Finish(ctx)
	return sum, s.decorate(err)
}

func (s *sshSession) Root() string { return s.client.RemoteRoot() }

func (s *sshSession) Close() error {
	s.once.Do(func() {
		if s.client != nil {
			_ = s.client.Close()
		}
		s.closed = s.cmd.Wait()
	})
	return s.closed
}

// decorate attaches remote stderr to protocol errors, because "unexpected EOF"
// on its own is useless when the real problem was "modelmove: command not
// found" on the far side.
func (s *sshSession) decorate(err error) error {
	if err == nil {
		return nil
	}
	if tail := s.stderr.String(); tail != "" {
		return fmt.Errorf("%w (remote: %s)", err, tail)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return fmt.Errorf("%w (the remote helper exited; is %q installed on the remote PATH?)",
			err, orDefault(s.cfg.RemoteBin, DefaultRemoteBinary))
	}
	return err
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// pipeCloser closes the helper's stdin, which is how it is asked to exit.
type pipeCloser struct {
	stdin io.WriteCloser
	cmd   *exec.Cmd
}

func (p *pipeCloser) Close() error { return p.stdin.Close() }

// stderrTail keeps the last few kilobytes of remote stderr.
type stderrTail struct {
	mu  sync.Mutex
	buf []byte
}

const stderrTailLimit = 4 << 10

func (t *stderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > stderrTailLimit {
		t.buf = t.buf[len(t.buf)-stderrTailLimit:]
	}
	return len(p), nil
}

func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// LookPathRemoteHint returns a hint about the local binary path, used when
// suggesting how to install the helper on the far side.
func LookPathRemoteHint() string {
	exe, err := os.Executable()
	if err != nil {
		return DefaultRemoteBinary
	}
	return exe
}
