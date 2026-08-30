// Package protocol defines the framed message stream modelmove speaks to a
// remote helper over SSH, plus the client and server that drive it.
package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/shaneburrell/modelmove/internal/chunk"
)

// Version is the wire protocol version. Both ends must agree exactly.
const Version = 1

// MaxFrame bounds a single frame payload. It has to comfortably exceed the
// largest chunk (16 MiB) and the largest manifest a helper will accept.
const MaxFrame = 256 << 20

// HeaderSize is the size of the frame header: one type byte and a length.
const HeaderSize = 5

// MsgType identifies a frame.
type MsgType uint8

// Frame types.
const (
	MsgHello MsgType = iota + 1
	MsgHelloAck
	MsgPlanReq      // JSON PlanRequest
	MsgManifest     // binary manifest
	MsgPlanResp     // JSON receiver.Plan
	MsgFileBegin    // JSON FileBegin
	MsgChunk        // 32-byte digest followed by the payload
	MsgFileEnd      // empty
	MsgFileResult   // JSON receiver.FileResult
	MsgFinish       // empty
	MsgSummary      // JSON receiver.Summary
	MsgError        // JSON Error
	MsgWarn         // JSON Warn
	MsgManifestGzip // gzip-compressed binary manifest (negotiated)
)

// FeatureManifestGzip is advertised in Hello/HelloAck when both ends can
// compress the planning-phase manifest. Version stays 1 so older helpers
// still work; they simply omit the feature and receive MsgManifest.
const FeatureManifestGzip = "manifest-gzip"

func (t MsgType) String() string {
	switch t {
	case MsgHello:
		return "hello"
	case MsgHelloAck:
		return "hello-ack"
	case MsgPlanReq:
		return "plan-req"
	case MsgManifest:
		return "manifest"
	case MsgManifestGzip:
		return "manifest-gzip"
	case MsgPlanResp:
		return "plan-resp"
	case MsgFileBegin:
		return "file-begin"
	case MsgChunk:
		return "chunk"
	case MsgFileEnd:
		return "file-end"
	case MsgFileResult:
		return "file-result"
	case MsgFinish:
		return "finish"
	case MsgSummary:
		return "summary"
	case MsgError:
		return "error"
	case MsgWarn:
		return "warn"
	default:
		return fmt.Sprintf("msg(%d)", uint8(t))
	}
}

// Hello is the first frame the client sends.
type Hello struct {
	Version  int      `json:"version"`
	Tool     string   `json:"tool"`
	Root     string   `json:"root"`
	Features []string `json:"features,omitempty"`
}

// HelloAck is the helper's reply.
type HelloAck struct {
	Version  int      `json:"version"`
	Tool     string   `json:"tool"`
	Root     string   `json:"root"`
	Features []string `json:"features,omitempty"`
}

func hasFeature(features []string, name string) bool {
	for _, f := range features {
		if f == name {
			return true
		}
	}
	return false
}

// PlanRequest carries the wire-safe subset of the receiver options.
type PlanRequest struct {
	Atomic        string `json:"atomic"`
	Dedupe        bool   `json:"dedupe"`
	Fast          bool   `json:"fast"`
	Resume        bool   `json:"resume"`
	Delete        bool   `json:"delete"`
	Verify        bool   `json:"verify"`
	PreserveTimes bool   `json:"preserve_times"`
	Jobs          int    `json:"jobs"`
}

// FileBegin announces the file whose chunks follow.
type FileBegin struct {
	Path string `json:"path"`
}

// Error is a fatal error reported by either end.
type Error struct {
	Message string `json:"message"`
}

func (e Error) Error() string { return e.Message }

// Warn is a non-fatal message forwarded from the helper.
type Warn struct {
	Message string `json:"message"`
}

// ErrFrameTooLarge is returned when a peer announces an oversized frame.
var ErrFrameTooLarge = errors.New("protocol: frame exceeds maximum size")

// WriteFrame writes one framed message.
func WriteFrame(w io.Writer, t MsgType, payload []byte) error {
	if len(payload) > MaxFrame {
		return ErrFrameTooLarge
	}
	var hdr [HeaderSize]byte
	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// ReadHeader reads a frame header without consuming the payload, which lets a
// large manifest be decoded as a stream instead of buffered.
func ReadHeader(r io.Reader) (MsgType, uint32, error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, 0, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFrame {
		return 0, 0, ErrFrameTooLarge
	}
	return MsgType(hdr[0]), n, nil
}

// ReadFrame reads one framed message.
func ReadFrame(r io.Reader) (MsgType, []byte, error) {
	t, n, err := ReadHeader(r)
	if err != nil {
		return 0, nil, err
	}
	if n == 0 {
		return t, nil, nil
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return t, payload, nil
}

// WriteJSON writes a frame whose payload is the JSON encoding of v.
func WriteJSON(w io.Writer, t MsgType, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, t, payload)
}

// DecodeJSON decodes a JSON frame payload.
func DecodeJSON(payload []byte, v any) error {
	if err := json.Unmarshal(payload, v); err != nil {
		return fmt.Errorf("protocol: decode: %w", err)
	}
	return nil
}

// EncodeChunk builds a chunk frame payload.
func EncodeChunk(d chunk.Digest, data []byte) []byte {
	out := make([]byte, chunk.DigestSize+len(data))
	copy(out, d[:])
	copy(out[chunk.DigestSize:], data)
	return out
}

// WriteChunkFrame writes a chunk frame without concatenating the digest and
// the payload into a temporary buffer.
func WriteChunkFrame(w io.Writer, d chunk.Digest, data []byte) error {
	n := chunk.DigestSize + len(data)
	if n > MaxFrame {
		return ErrFrameTooLarge
	}
	var hdr [HeaderSize + chunk.DigestSize]byte
	hdr[0] = byte(MsgChunk)
	binary.BigEndian.PutUint32(hdr[1:], uint32(n))
	copy(hdr[HeaderSize:], d[:])
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// DecodeChunk splits a chunk frame payload. The returned slice aliases payload.
func DecodeChunk(payload []byte) (chunk.Digest, []byte, error) {
	var d chunk.Digest
	if len(payload) < chunk.DigestSize {
		return d, nil, errors.New("protocol: short chunk frame")
	}
	copy(d[:], payload[:chunk.DigestSize])
	return d, payload[chunk.DigestSize:], nil
}

// WriteError sends a fatal error frame.
func WriteError(w io.Writer, err error) error {
	return WriteJSON(w, MsgError, Error{Message: err.Error()})
}

// asError turns an error frame payload into an error value.
func asError(payload []byte) error {
	var e Error
	if err := DecodeJSON(payload, &e); err != nil {
		return err
	}
	if e.Message == "" {
		e.Message = "remote reported an unspecified error"
	}
	return e
}
