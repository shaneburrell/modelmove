package transport

import (
	"context"

	"github.com/shaneburrell/modelmove/internal/chunk"
	"github.com/shaneburrell/modelmove/internal/manifest"
	"github.com/shaneburrell/modelmove/internal/receiver"
)

// localSession applies a manifest in this process. It is the same receiver the
// SSH helper runs, minus the framing.
type localSession struct {
	recv *receiver.Receiver
}

func newLocalSession(e Endpoint, cfg Config) (Session, error) {
	path, err := e.LocalPath()
	if err != nil {
		return nil, err
	}
	recv, err := receiver.New(receiver.Options{
		Root:          path,
		Atomic:        cfg.Atomic,
		Dedupe:        cfg.Dedupe,
		Fast:          cfg.Fast,
		Resume:        cfg.Resume,
		Delete:        cfg.Delete,
		Verify:        cfg.Verify,
		PreserveTimes: cfg.PreserveTimes,
		Pin:           cfg.Pin,
		Jobs:          cfg.Jobs,
		Bar:           cfg.Bar,
		Warn:          cfg.Warn,
	})
	if err != nil {
		return nil, err
	}
	return &localSession{recv: recv}, nil
}

func (s *localSession) Plan(ctx context.Context, m *manifest.Manifest) (*receiver.Plan, error) {
	return s.recv.Plan(ctx, m)
}

func (s *localSession) BeginFile(_ context.Context, rel string) error {
	return s.recv.BeginFile(rel)
}

func (s *localSession) SendChunk(_ context.Context, d chunk.Digest, data []byte) error {
	return s.recv.Chunk(d, data)
}

func (s *localSession) EndFile(_ context.Context) (*receiver.FileResult, error) {
	return s.recv.EndFile()
}

func (s *localSession) Finish(_ context.Context) (*receiver.Summary, error) {
	return s.recv.Finish()
}

func (s *localSession) Root() string { return s.recv.Root() }

func (s *localSession) Close() error {
	s.recv.Abort()
	return nil
}
