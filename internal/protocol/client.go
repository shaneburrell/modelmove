package protocol

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/shaneburrell/modelmove/internal/chunk"
	"github.com/shaneburrell/modelmove/internal/manifest"
	"github.com/shaneburrell/modelmove/internal/receiver"
)

// Client drives a remote helper over a pair of pipes.
type Client struct {
	r      *bufio.Reader
	w      *bufio.Writer
	closer io.Closer
	tool   string
	warn   func(format string, args ...any)
	root   string
}

// ClientOptions configures a Client.
type ClientOptions struct {
	Tool string
	Root string
	Warn func(format string, args ...any)
}

// NewClient wraps a reader/writer pair, typically the stdio of an ssh
// subprocess, and completes the handshake.
func NewClient(r io.Reader, w io.Writer, closer io.Closer, opt ClientOptions) (*Client, error) {
	c := &Client{
		r:      bufio.NewReaderSize(r, 1<<20),
		w:      bufio.NewWriterSize(w, 1<<20),
		closer: closer,
		tool:   opt.Tool,
		warn:   opt.Warn,
		root:   opt.Root,
	}
	if err := c.send(MsgHello, Hello{Version: Version, Tool: opt.Tool, Root: opt.Root}); err != nil {
		return nil, err
	}
	payload, err := c.expect(MsgHelloAck)
	if err != nil {
		return nil, err
	}
	var ack HelloAck
	if err := DecodeJSON(payload, &ack); err != nil {
		return nil, err
	}
	if ack.Version != Version {
		return nil, fmt.Errorf("protocol: remote speaks version %d, this build speaks %d", ack.Version, Version)
	}
	c.root = ack.Root
	return c, nil
}

// RemoteRoot returns the absolute destination path reported by the helper.
func (c *Client) RemoteRoot() string { return c.root }

func (c *Client) send(t MsgType, v any) error {
	if err := WriteJSON(c.w, t, v); err != nil {
		return err
	}
	return c.w.Flush()
}

// next reads the next frame, transparently forwarding warnings and turning
// error frames into Go errors.
func (c *Client) next() (MsgType, []byte, error) {
	for {
		t, payload, err := ReadFrame(c.r)
		if err != nil {
			return 0, nil, err
		}
		switch t {
		case MsgWarn:
			var w Warn
			if err := DecodeJSON(payload, &w); err == nil && c.warn != nil {
				c.warn("remote: %s", w.Message)
			}
		case MsgError:
			return 0, nil, asError(payload)
		default:
			return t, payload, nil
		}
	}
}

func (c *Client) expect(want MsgType) ([]byte, error) {
	t, payload, err := c.next()
	if err != nil {
		return nil, err
	}
	if t != want {
		return nil, fmt.Errorf("protocol: expected %s, got %s", want, t)
	}
	return payload, nil
}

// Plan sends the manifest and returns the helper's transfer plan.
func (c *Client) Plan(ctx context.Context, m *manifest.Manifest, req PlanRequest) (*receiver.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.send(MsgPlanReq, req); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := manifest.EncodeBinary(&buf, m); err != nil {
		return nil, err
	}
	if err := WriteFrame(c.w, MsgManifest, buf.Bytes()); err != nil {
		return nil, err
	}
	if err := c.w.Flush(); err != nil {
		return nil, err
	}
	payload, err := c.expect(MsgPlanResp)
	if err != nil {
		return nil, err
	}
	var plan receiver.Plan
	if err := DecodeJSON(payload, &plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

// BeginFile announces the next file.
func (c *Client) BeginFile(ctx context.Context, rel string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.send(MsgFileBegin, FileBegin{Path: rel})
}

// SendChunk streams one chunk payload. The digest and the data are written
// straight to the buffered writer rather than being concatenated first, which
// matters when chunks are 16 MiB.
func (c *Client) SendChunk(ctx context.Context, d chunk.Digest, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return WriteChunkFrame(c.w, d, data)
}

// EndFile closes the current file and waits for its result.
func (c *Client) EndFile(ctx context.Context) (*receiver.FileResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := WriteFrame(c.w, MsgFileEnd, nil); err != nil {
		return nil, err
	}
	if err := c.w.Flush(); err != nil {
		return nil, err
	}
	payload, err := c.expect(MsgFileResult)
	if err != nil {
		return nil, err
	}
	var res receiver.FileResult
	if err := DecodeJSON(payload, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Finish tells the helper to commit and returns the summary.
func (c *Client) Finish(ctx context.Context) (*receiver.Summary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := WriteFrame(c.w, MsgFinish, nil); err != nil {
		return nil, err
	}
	if err := c.w.Flush(); err != nil {
		return nil, err
	}
	payload, err := c.expect(MsgSummary)
	if err != nil {
		return nil, err
	}
	var sum receiver.Summary
	if err := DecodeJSON(payload, &sum); err != nil {
		return nil, err
	}
	return &sum, nil
}

// Close shuts down the underlying transport.
func (c *Client) Close() error {
	if c.closer == nil {
		return nil
	}
	return c.closer.Close()
}
