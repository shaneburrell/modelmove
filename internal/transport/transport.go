// Package transport resolves a destination string to a session that applies a
// manifest, either in this process or on a remote host over SSH.
package transport

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/shaneburrell/modelmove/internal/chunk"
	"github.com/shaneburrell/modelmove/internal/manifest"
	"github.com/shaneburrell/modelmove/internal/progress"
	"github.com/shaneburrell/modelmove/internal/receiver"
)

// Scheme identifies a destination kind.
type Scheme string

// Supported schemes.
const (
	SchemeFile Scheme = "file"
	SchemeSSH  Scheme = "ssh"
)

// Endpoint is a parsed source or destination.
type Endpoint struct {
	Scheme Scheme
	User   string
	Host   string
	Port   int
	Path   string
	Raw    string
}

// IsLocal reports whether the endpoint lives on this machine.
func (e Endpoint) IsLocal() bool { return e.Scheme == SchemeFile }

func (e Endpoint) String() string {
	if e.IsLocal() {
		return e.Path
	}
	var b strings.Builder
	b.WriteString("ssh://")
	if e.User != "" {
		b.WriteString(e.User + "@")
	}
	b.WriteString(e.Host)
	if e.Port != 0 {
		fmt.Fprintf(&b, ":%d", e.Port)
	}
	b.WriteString("/" + strings.TrimPrefix(e.Path, "/"))
	return b.String()
}

// Parse understands local paths, "host:/path", "user@host:/path" and
// "ssh://user@host:port/path".
func Parse(s string) (Endpoint, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Endpoint{}, fmt.Errorf("transport: empty path")
	}

	if rest, ok := cutPrefixFold(raw, "ssh://"); ok {
		return parseSSHURL(raw, rest)
	}
	if rest, ok := cutPrefixFold(raw, "file://"); ok {
		return Endpoint{Scheme: SchemeFile, Path: rest, Raw: raw}, nil
	}

	// "host:/path" is remote, but "C:\models" and "./a:b" are not.
	if i := strings.Index(raw, ":"); i > 0 && !strings.HasPrefix(raw, ".") && !strings.HasPrefix(raw, "/") {
		hostPart := raw[:i]
		pathPart := raw[i+1:]
		if !isWindowsDrive(hostPart) && !strings.ContainsAny(hostPart, `/\`) && pathPart != "" {
			user, host := splitUser(hostPart)
			return Endpoint{Scheme: SchemeSSH, User: user, Host: host, Path: pathPart, Raw: raw}, nil
		}
	}
	return Endpoint{Scheme: SchemeFile, Path: raw, Raw: raw}, nil
}

func parseSSHURL(raw, rest string) (Endpoint, error) {
	e := Endpoint{Scheme: SchemeSSH, Raw: raw}
	authority := rest
	if i := strings.Index(rest, "/"); i >= 0 {
		authority = rest[:i]
		e.Path = rest[i:]
	}
	if authority == "" {
		return Endpoint{}, fmt.Errorf("transport: %q has no host", raw)
	}
	e.User, authority = splitUser(authority)
	if i := strings.LastIndex(authority, ":"); i >= 0 {
		port, err := strconv.Atoi(authority[i+1:])
		if err != nil || port <= 0 || port > 65535 {
			return Endpoint{}, fmt.Errorf("transport: %q has an invalid port", raw)
		}
		e.Port = port
		authority = authority[:i]
	}
	e.Host = authority
	if e.Host == "" {
		return Endpoint{}, fmt.Errorf("transport: %q has no host", raw)
	}
	if e.Path == "" {
		return Endpoint{}, fmt.Errorf("transport: %q has no path", raw)
	}
	return e, nil
}

func splitUser(s string) (user, rest string) {
	if i := strings.LastIndex(s, "@"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "", s
}

func isWindowsDrive(s string) bool {
	return runtime.GOOS == "windows" && len(s) == 1 && isLetter(s[0])
}

func isLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// LocalPath returns the cleaned filesystem path of a local endpoint.
func (e Endpoint) LocalPath() (string, error) {
	if !e.IsLocal() {
		return "", fmt.Errorf("transport: %s is not a local path", e)
	}
	if strings.TrimSpace(e.Path) == "" {
		return "", fmt.Errorf("transport: empty local path")
	}
	return filepath.Clean(e.Path), nil
}

// Session applies a manifest to a destination. Chunks are pushed by the
// caller; everything else is decided by the destination.
type Session interface {
	// Plan reports what has to be transferred.
	Plan(ctx context.Context, m *manifest.Manifest) (*receiver.Plan, error)
	// BeginFile opens the next file for writing.
	BeginFile(ctx context.Context, rel string) error
	// SendChunk delivers one chunk payload for the open file.
	SendChunk(ctx context.Context, d chunk.Digest, data []byte) error
	// EndFile completes the open file.
	EndFile(ctx context.Context) (*receiver.FileResult, error)
	// Finish commits the transfer.
	Finish(ctx context.Context) (*receiver.Summary, error)
	// Root returns the absolute destination path.
	Root() string
	// Close releases the session.
	Close() error
}

// Config carries the destination settings shared by every transport.
type Config struct {
	Atomic        receiver.AtomicMode
	Dedupe        bool
	Fast          bool
	Resume        bool
	Delete        bool
	Verify        bool
	PreserveTimes bool
	Jobs          int
	Pin           chunk.Options
	Bar           *progress.Bar
	Warn          func(format string, args ...any)

	// SSH settings, ignored for local destinations.
	SSHCommand string
	SSHOptions []string
	RemoteBin  string
	Tool       string
}

// Dial opens a session for the destination endpoint.
func Dial(ctx context.Context, e Endpoint, cfg Config) (Session, error) {
	switch e.Scheme {
	case SchemeFile:
		return newLocalSession(e, cfg)
	case SchemeSSH:
		return newSSHSession(ctx, e, cfg)
	default:
		return nil, fmt.Errorf("transport: unsupported scheme %q", e.Scheme)
	}
}
