package protocol

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/shaneburrell/modelmove/internal/chunk"
	"github.com/shaneburrell/modelmove/internal/manifest"
	"github.com/shaneburrell/modelmove/internal/receiver"
	"github.com/shaneburrell/modelmove/internal/scan"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte("hello")
	if err := WriteFrame(&buf, MsgHello, payload); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if err := WriteFrame(&buf, MsgFileEnd, nil); err != nil {
		t.Fatalf("WriteFrame empty: %v", err)
	}

	typ, got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if typ != MsgHello || string(got) != "hello" {
		t.Fatalf("got (%v, %q)", typ, got)
	}
	typ, got, err = ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame empty: %v", err)
	}
	if typ != MsgFileEnd || len(got) != 0 {
		t.Fatalf("empty frame = (%v, %q)", typ, got)
	}
}

func TestFrameTooLarge(t *testing.T) {
	if err := WriteFrame(io.Discard, MsgHello, make([]byte, MaxFrame+1)); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("WriteFrame oversized = %v", err)
	}
	// A header claiming more than the maximum must be rejected before any
	// allocation happens.
	hdr := []byte{byte(MsgHello), 0xff, 0xff, 0xff, 0xff}
	if _, _, err := ReadFrame(bytes.NewReader(hdr)); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("ReadFrame oversized = %v", err)
	}
}

func TestReadFrameTruncated(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, MsgHello, []byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	truncated := buf.Bytes()[:buf.Len()-2]
	if _, _, err := ReadFrame(bytes.NewReader(truncated)); err == nil {
		t.Error("ReadFrame accepted a truncated payload")
	}
	if _, _, err := ReadFrame(bytes.NewReader(nil)); err == nil {
		t.Error("ReadFrame accepted an empty stream")
	}
}

func TestChunkFrame(t *testing.T) {
	data := []byte("tensor bytes")
	d := chunk.Sum(data)

	var buf bytes.Buffer
	if err := WriteChunkFrame(&buf, d, data); err != nil {
		t.Fatalf("WriteChunkFrame: %v", err)
	}
	typ, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if typ != MsgChunk {
		t.Fatalf("type = %v", typ)
	}
	gotDigest, gotData, err := DecodeChunk(payload)
	if err != nil {
		t.Fatal(err)
	}
	if gotDigest != d || string(gotData) != string(data) {
		t.Fatal("chunk frame round trip changed the payload")
	}

	if !bytes.Equal(EncodeChunk(d, data), payload) {
		t.Error("EncodeChunk and WriteChunkFrame disagree")
	}
	if _, _, err := DecodeChunk([]byte("short")); err == nil {
		t.Error("DecodeChunk accepted a short payload")
	}
	if err := WriteChunkFrame(io.Discard, d, make([]byte, MaxFrame)); !errors.Is(err, ErrFrameTooLarge) {
		t.Error("WriteChunkFrame accepted an oversized chunk")
	}
}

func TestJSONFrames(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, MsgHello, Hello{Version: Version, Tool: "t", Root: "/r"}); err != nil {
		t.Fatal(err)
	}
	_, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	var h Hello
	if err := DecodeJSON(payload, &h); err != nil {
		t.Fatal(err)
	}
	if h.Root != "/r" || h.Version != Version {
		t.Fatalf("decoded %+v", h)
	}
	if err := DecodeJSON([]byte("{"), &h); err == nil {
		t.Error("DecodeJSON accepted invalid JSON")
	}
	if err := WriteJSON(io.Discard, MsgHello, make(chan int)); err == nil {
		t.Error("WriteJSON accepted an unmarshalable value")
	}
}

func TestErrorFrame(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteError(&buf, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	_, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := asError(payload); got.Error() != "boom" {
		t.Errorf("asError = %v", got)
	}
	if got := asError([]byte(`{"message":""}`)); got == nil || got.Error() == "" {
		t.Error("an empty error message should still produce an error")
	}
	if got := asError([]byte("not json")); got == nil {
		t.Error("a malformed error frame should still produce an error")
	}
}

func TestMsgTypeString(t *testing.T) {
	for _, m := range []MsgType{MsgHello, MsgHelloAck, MsgPlanReq, MsgManifest, MsgPlanResp,
		MsgFileBegin, MsgChunk, MsgFileEnd, MsgFileResult, MsgFinish, MsgSummary, MsgError, MsgWarn, MsgManifestGzip} {
		if m.String() == "" {
			t.Errorf("MsgType(%d) has no name", m)
		}
	}
	if got := MsgType(200).String(); got != "msg(200)" {
		t.Errorf("unknown type = %q", got)
	}
}

// pipePair wires a client and a server together over two in-memory pipes,
// exercising the real framing without spawning ssh.
func pipePair(t *testing.T, opt ServerOptions) (*Client, func()) {
	t.Helper()
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()

	var (
		wg      sync.WaitGroup
		serveMu sync.Mutex
		serveEr error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := Serve(context.Background(), serverReader, serverWriter, opt)
		serveMu.Lock()
		serveEr = err
		serveMu.Unlock()
		serverWriter.Close()
	}()

	client, err := NewClient(clientReader, clientWriter, clientWriter, ClientOptions{
		Tool: "modelmove/test",
		Root: opt.Root,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client, func() {
		clientWriter.Close()
		wg.Wait()
		serveMu.Lock()
		defer serveMu.Unlock()
		if serveEr != nil && !errors.Is(serveEr, io.ErrClosedPipe) {
			t.Logf("server returned: %v", serveEr)
		}
	}
}

func randomBytes(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	r.Read(b)
	return b
}

func sourceDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "config.json", []byte(`{"model_type":"llama"}`))
	writeFile(t, dir, "model-00001-of-00002.safetensors", randomBytes(200<<10, 1))
	writeFile(t, dir, "model-00002-of-00002.safetensors", randomBytes(200<<10, 2))
	return dir
}

func writeFile(t *testing.T, dir, rel string, data []byte) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// runTransfer drives a complete transfer through the protocol.
func runTransfer(t *testing.T, client *Client, m *manifest.Manifest, src string, req PlanRequest) (*receiver.Plan, *receiver.Summary) {
	t.Helper()
	ctx := context.Background()
	plan, err := client.Plan(ctx, m, req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, fp := range plan.Files {
		if fp.Action == receiver.ActionSkip {
			continue
		}
		if err := client.BeginFile(ctx, fp.Path); err != nil {
			t.Fatalf("BeginFile: %v", err)
		}
		f := m.Lookup(fp.Path)
		fh, err := os.Open(filepath.Join(src, filepath.FromSlash(fp.Path)))
		if err != nil {
			t.Fatal(err)
		}
		where := map[chunk.Digest]manifest.Chunk{}
		for _, c := range f.Chunks {
			if _, ok := where[c.Digest]; !ok {
				where[c.Digest] = c
			}
		}
		for _, d := range fp.Need {
			c := where[d]
			buf := make([]byte, c.Length)
			if _, err := fh.ReadAt(buf, int64(c.Offset)); err != nil {
				t.Fatal(err)
			}
			if err := client.SendChunk(ctx, d, buf); err != nil {
				t.Fatalf("SendChunk: %v", err)
			}
		}
		fh.Close()
		res, err := client.EndFile(ctx)
		if err != nil {
			t.Fatalf("EndFile: %v", err)
		}
		if res.Status != "ok" {
			t.Fatalf("%s: %s", res.Path, res.Error)
		}
	}
	sum, err := client.Finish(ctx)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return plan, sum
}

func defaultRequest() PlanRequest {
	return PlanRequest{Atomic: "file", Dedupe: true, Resume: true, Verify: true, PreserveTimes: true, Jobs: 2}
}

func TestFullTransferOverPipes(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")

	m, err := scan.Build(context.Background(), scan.Options{Root: src, Tool: "test"})
	if err != nil {
		t.Fatal(err)
	}

	client, done := pipePair(t, ServerOptions{Root: dst, Tool: "modelmove/test"})
	plan, sum := runTransfer(t, client, m, src, defaultRequest())
	done()

	if plan.CopyFiles != 3 {
		t.Errorf("CopyFiles = %d, want 3", plan.CopyFiles)
	}
	if sum.FilesWritten != 3 {
		t.Errorf("FilesWritten = %d, want 3", sum.FilesWritten)
	}
	if client.RemoteRoot() == "" {
		t.Error("RemoteRoot is empty")
	}
	for _, rel := range []string{"config.json", "model-00001-of-00002.safetensors"} {
		want, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want, got) {
			t.Errorf("%s does not match the source", rel)
		}
	}
}

func TestSparseTransferOverPipes(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")

	build := func() *manifest.Manifest {
		m, err := scan.Build(context.Background(), scan.Options{Root: src, Tool: "test"})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	client, done := pipePair(t, ServerOptions{Root: dst})
	runTransfer(t, client, build(), src, defaultRequest())
	done()

	// Edit a few bytes in the middle of a shard.
	path := filepath.Join(src, "model-00001-of-00002.safetensors")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	copy(data[100<<10:], []byte("EDITED"))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	client, done = pipePair(t, ServerOptions{Root: dst})
	plan, sum := runTransfer(t, client, build(), src, defaultRequest())
	done()

	if plan.NeedBytes >= plan.TotalBytes/4 {
		t.Errorf("a 6-byte edit moved %d of %d bytes over the wire", plan.NeedBytes, plan.TotalBytes)
	}
	if sum.BytesReused == 0 {
		t.Error("the remote reused nothing")
	}
	got, err := os.ReadFile(filepath.Join(dst, "model-00001-of-00002.safetensors"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("the updated file does not match the source")
	}
}

func TestServerRejectsDeleteWithoutPermission(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	m, err := scan.Build(context.Background(), scan.Options{Root: src})
	if err != nil {
		t.Fatal(err)
	}

	client, done := pipePair(t, ServerOptions{Root: dst, AllowDelete: false})
	req := defaultRequest()
	req.Delete = true
	_, err = client.Plan(context.Background(), m, req)
	done()

	if err == nil {
		t.Fatal("the helper allowed a delete it was not started for")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("allow-delete")) {
		t.Errorf("error = %v, want it to mention --allow-delete", err)
	}
}

func TestServerRejectsEscapingPath(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out")
	src := t.TempDir()
	writeFile(t, src, "a.safetensors", randomBytes(4096, 3))
	m, err := scan.Build(context.Background(), scan.Options{Root: src})
	if err != nil {
		t.Fatal(err)
	}

	client, done := pipePair(t, ServerOptions{Root: dst})
	if _, err := client.Plan(context.Background(), m, defaultRequest()); err != nil {
		t.Fatal(err)
	}
	err = client.BeginFile(context.Background(), "../../etc/passwd")
	if err == nil {
		// The failure may surface on the next read instead of the write.
		_, err = client.EndFile(context.Background())
	}
	done()
	if err == nil {
		t.Fatal("the helper accepted a path outside its root")
	}
}

func TestServerRejectsVersionMismatch(t *testing.T) {
	serverReader, clientWriter := io.Pipe()
	clientReader, serverWriter := io.Pipe()

	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), serverReader, serverWriter, ServerOptions{Root: t.TempDir()})
		serverWriter.Close()
	}()

	if err := WriteJSON(clientWriter, MsgHello, Hello{Version: 999, Tool: "old"}); err != nil {
		t.Fatal(err)
	}
	typ, payload, err := ReadFrame(clientReader)
	if err != nil {
		t.Fatal(err)
	}
	if typ != MsgError {
		t.Fatalf("expected an error frame, got %v", typ)
	}
	if got := asError(payload).Error(); got == "" {
		t.Error("empty error message")
	}
	clientWriter.Close()
	<-done
}

func TestClientRejectsNonHelloAck(t *testing.T) {
	clientReader, serverWriter := io.Pipe()

	go func() {
		_ = WriteJSON(serverWriter, MsgSummary, receiver.Summary{})
		serverWriter.Close()
	}()
	if _, err := NewClient(clientReader, io.Discard, nil, ClientOptions{}); err == nil {
		t.Fatal("NewClient accepted a bad handshake")
	}
}

func TestClientForwardsWarnings(t *testing.T) {
	clientReader, serverWriter := io.Pipe()
	clientWriter := io.Discard

	go func() {
		_ = WriteJSON(serverWriter, MsgWarn, Warn{Message: "disk is nearly full"})
		_ = WriteJSON(serverWriter, MsgHelloAck, HelloAck{Version: Version, Root: "/r"})
		serverWriter.Close()
	}()

	var warnings []string
	c, err := NewClient(clientReader, clientWriter, nil, ClientOptions{
		Warn: func(format string, args ...any) { warnings = append(warnings, format) },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if len(warnings) != 1 {
		t.Errorf("got %d warnings, want 1", len(warnings))
	}
	if c.RemoteRoot() != "/r" {
		t.Errorf("RemoteRoot = %q", c.RemoteRoot())
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestServerRejectsOutOfOrderFrames(t *testing.T) {
	client, done := pipePair(t, ServerOptions{Root: t.TempDir()})
	err := client.BeginFile(context.Background(), "x")
	if err == nil {
		_, err = client.EndFile(context.Background())
	}
	done()
	if err == nil {
		t.Fatal("the helper accepted a file before a plan")
	}
}

func TestCancelledContextStopsClient(t *testing.T) {
	client, done := pipePair(t, ServerOptions{Root: t.TempDir()})
	defer done()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Plan(ctx, manifest.New("t", chunk.SmallFileOptions()), defaultRequest()); err == nil {
		t.Error("Plan ignored a cancelled context")
	}
	if err := client.BeginFile(ctx, "x"); err == nil {
		t.Error("BeginFile ignored a cancelled context")
	}
	if err := client.SendChunk(ctx, chunk.Digest{}, nil); err == nil {
		t.Error("SendChunk ignored a cancelled context")
	}
	if _, err := client.EndFile(ctx); err == nil {
		t.Error("EndFile ignored a cancelled context")
	}
	if _, err := client.Finish(ctx); err == nil {
		t.Error("Finish ignored a cancelled context")
	}
}

func TestGzipManifestRoundTrip(t *testing.T) {
	src := sourceDir(t)
	m, err := scan.Build(context.Background(), scan.Options{Root: src, Tool: "test"})
	if err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	if err := manifest.EncodeBinary(&raw, m); err != nil {
		t.Fatal(err)
	}
	gz, err := gzipBytes(raw.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(gz) == 0 || bytes.Equal(gz, raw.Bytes()) {
		t.Fatal("gzip did not change the payload")
	}
	got, err := gunzipBytes(gz)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw.Bytes()) {
		t.Fatal("gunzip did not restore the manifest bytes")
	}
}

func TestManifestGzipOverPipes(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	m, err := scan.Build(context.Background(), scan.Options{Root: src, Tool: "test"})
	if err != nil {
		t.Fatal(err)
	}
	client, done := pipePair(t, ServerOptions{Root: dst, Tool: "modelmove/test"})
	if !client.manifestGzip {
		done()
		t.Fatal("handshake did not negotiate manifest-gzip")
	}
	runTransfer(t, client, m, src, defaultRequest())
	done()
	want, err := os.ReadFile(filepath.Join(src, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, got) {
		t.Fatal("gzip plan transfer did not write the source file")
	}
}

func TestResumeOverPipes(t *testing.T) {
	src := t.TempDir()
	payload := randomBytes(400<<10, 6)
	writeFile(t, src, "model.safetensors", payload)
	m, err := scan.Build(context.Background(), scan.Options{Root: src, Tool: "test"})
	if err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "out")
	stage := filepath.Join(dst, manifest.StateDir, "stage", "model.safetensors.part")
	if err := os.MkdirAll(filepath.Dir(stage), 0o755); err != nil {
		t.Fatal(err)
	}
	partial := make([]byte, len(payload))
	f := m.Files[0]
	var filled int
	for _, c := range f.Chunks {
		if filled >= len(payload)/2 {
			break
		}
		copy(partial[c.Offset:], payload[c.Offset:c.End()])
		filled += int(c.Length)
	}
	if err := os.WriteFile(stage, partial, 0o644); err != nil {
		t.Fatal(err)
	}

	client, done := pipePair(t, ServerOptions{Root: dst, Tool: "modelmove/test"})
	plan, _ := runTransfer(t, client, m, src, defaultRequest())
	done()

	if plan.NeedBytes >= int64(len(payload)) {
		t.Errorf("resume sent %d of %d bytes; staged chunks should have been kept", plan.NeedBytes, len(payload))
	}
	got, err := os.ReadFile(filepath.Join(dst, "model.safetensors"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("the resumed file does not match the source")
	}
}

func TestPlanFailsWhenDestUnhashableOverPipes(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "model.safetensors", randomBytes(64<<10, 21))
	m, err := scan.Build(context.Background(), scan.Options{Root: src, Tool: "test"})
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	client, done := pipePair(t, ServerOptions{Root: dst, Tool: "modelmove/test"})
	runTransfer(t, client, m, src, defaultRequest())
	done()

	m.Files[0].Chunker.MinSize = 8 << 20
	m.Files[0].Chunker.AvgSize = 1 << 20
	m.Files[0].Chunker.MaxSize = 4 << 20

	client, done = pipePair(t, ServerOptions{Root: dst, Tool: "modelmove/test"})
	_, err = client.Plan(context.Background(), m, defaultRequest())
	done()
	if err == nil {
		t.Fatal("Plan succeeded; want the dest hash failure over the wire")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("cannot hash destination file")) {
		t.Fatalf("error = %v, want it to mention cannot hash destination file", err)
	}
}

func TestCorruptionRepairOverPipes(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	m, err := scan.Build(context.Background(), scan.Options{Root: src, Tool: "test"})
	if err != nil {
		t.Fatal(err)
	}

	client, done := pipePair(t, ServerOptions{Root: dst, Tool: "modelmove/test"})
	runTransfer(t, client, m, src, defaultRequest())
	done()

	rel := "model-00002-of-00002.safetensors"
	path := filepath.Join(dst, rel)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	client, done = pipePair(t, ServerOptions{Root: dst, Tool: "modelmove/test"})
	plan, _ := runTransfer(t, client, m, src, defaultRequest())
	done()
	if plan.NeedBytes == 0 {
		t.Fatal("repair planned no bytes after dest corruption")
	}
	want, err := os.ReadFile(filepath.Join(src, rel))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, got) {
		t.Fatal("repair did not restore the source bytes")
	}
}
