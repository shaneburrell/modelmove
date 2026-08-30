package protocol

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/shaneburrell/modelmove/internal/manifest"
	"github.com/shaneburrell/modelmove/internal/receiver"
)

// ServerOptions configures a remote helper.
type ServerOptions struct {
	// Root is the destination directory the helper is confined to.
	Root string
	// Tool is reported in the handshake.
	Tool string
	// AllowDelete refuses delete requests unless set, so that a client cannot
	// remove files on a host that did not opt in.
	AllowDelete bool
}

// Serve runs the remote helper loop against a reader/writer pair, normally the
// process stdin and stdout under ssh.
func Serve(ctx context.Context, r io.Reader, w io.Writer, opt ServerOptions) error {
	br := bufio.NewReaderSize(r, 1<<20)
	bw := bufio.NewWriterSize(w, 1<<20)
	defer bw.Flush()

	s := &server{r: br, w: bw, opt: opt}
	if err := s.run(ctx); err != nil {
		// The client may still be streaming chunks or a large manifest at us.
		// If we stop reading, its writes block, it never reads our error, and
		// both ends wait forever. Drain in the background so it can drift to
		// the error frame.
		go func() { _, _ = io.Copy(io.Discard, br) }()
		_ = WriteError(bw, err)
		_ = bw.Flush()
		return err
	}
	return bw.Flush()
}

type server struct {
	r    *bufio.Reader
	w    *bufio.Writer
	opt  ServerOptions
	recv *receiver.Receiver
	root string
}

func (s *server) run(ctx context.Context) error {
	root, err := filepath.Abs(s.opt.Root)
	if err != nil {
		return err
	}
	s.root = root

	t, payload, err := ReadFrame(s.r)
	if err != nil {
		return fmt.Errorf("protocol: handshake: %w", err)
	}
	if t != MsgHello {
		return fmt.Errorf("protocol: expected hello, got %s", t)
	}
	var hello Hello
	if err := DecodeJSON(payload, &hello); err != nil {
		return err
	}
	if hello.Version != Version {
		return fmt.Errorf("protocol: client speaks version %d, this helper speaks %d", hello.Version, Version)
	}
	if err := s.reply(MsgHelloAck, HelloAck{
		Version:  Version,
		Tool:     s.opt.Tool,
		Root:     root,
		Features: []string{FeatureManifestGzip},
	}); err != nil {
		return err
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		t, payload, err := ReadFrame(s.r)
		if errors.Is(err, io.EOF) {
			s.abort()
			return nil
		}
		if err != nil {
			s.abort()
			return err
		}
		switch t {
		case MsgPlanReq:
			if err := s.handlePlan(ctx, payload); err != nil {
				return err
			}
		case MsgFileBegin:
			if err := s.handleFileBegin(payload); err != nil {
				return err
			}
		case MsgChunk:
			if err := s.handleChunk(payload); err != nil {
				return err
			}
		case MsgFileEnd:
			if err := s.handleFileEnd(); err != nil {
				return err
			}
		case MsgFinish:
			return s.handleFinish()
		default:
			return fmt.Errorf("protocol: unexpected frame %s", t)
		}
	}
}

func (s *server) abort() {
	if s.recv != nil {
		s.recv.Abort()
	}
}

func (s *server) reply(t MsgType, v any) error {
	if err := WriteJSON(s.w, t, v); err != nil {
		return err
	}
	return s.w.Flush()
}

func (s *server) warn(format string, args ...any) {
	_ = WriteJSON(s.w, MsgWarn, Warn{Message: fmt.Sprintf(format, args...)})
}

func (s *server) handlePlan(ctx context.Context, payload []byte) error {
	var req PlanRequest
	if err := DecodeJSON(payload, &req); err != nil {
		return err
	}
	if req.Delete && !s.opt.AllowDelete {
		return fmt.Errorf("protocol: this helper was not started with --allow-delete")
	}

	t, raw, err := ReadFrame(s.r)
	if err != nil {
		return err
	}
	switch t {
	case MsgManifestGzip:
		raw, err = gunzipBytes(raw)
		if err != nil {
			return err
		}
	case MsgManifest:
	default:
		return fmt.Errorf("protocol: expected manifest, got %s", t)
	}
	m, err := manifest.DecodeBinary(bytes.NewReader(raw))
	if err != nil {
		return err
	}

	atomic, err := receiver.ParseAtomicMode(req.Atomic)
	if err != nil {
		return err
	}
	s.recv, err = receiver.New(receiver.Options{
		Root:          s.root,
		Atomic:        atomic,
		Dedupe:        req.Dedupe,
		Fast:          req.Fast,
		Resume:        req.Resume,
		Delete:        req.Delete,
		Verify:        req.Verify,
		PreserveTimes: req.PreserveTimes,
		Jobs:          req.Jobs,
		Warn:          s.warn,
	})
	if err != nil {
		return err
	}
	plan, err := s.recv.Plan(ctx, m)
	if err != nil {
		return err
	}
	return s.reply(MsgPlanResp, plan)
}

func (s *server) handleFileBegin(payload []byte) error {
	if s.recv == nil {
		return fmt.Errorf("protocol: file-begin before plan")
	}
	var fb FileBegin
	if err := DecodeJSON(payload, &fb); err != nil {
		return err
	}
	return s.recv.BeginFile(fb.Path)
}

func (s *server) handleChunk(payload []byte) error {
	if s.recv == nil {
		return fmt.Errorf("protocol: chunk before plan")
	}
	d, data, err := DecodeChunk(payload)
	if err != nil {
		return err
	}
	return s.recv.Chunk(d, data)
}

func (s *server) handleFileEnd() error {
	if s.recv == nil {
		return fmt.Errorf("protocol: file-end before plan")
	}
	res, err := s.recv.EndFile()
	if err != nil {
		if res != nil {
			_ = s.reply(MsgFileResult, res)
		}
		return err
	}
	return s.reply(MsgFileResult, res)
}

func (s *server) handleFinish() error {
	if s.recv == nil {
		return fmt.Errorf("protocol: finish before plan")
	}
	sum, err := s.recv.Finish()
	if err != nil {
		return err
	}
	return s.reply(MsgSummary, sum)
}
