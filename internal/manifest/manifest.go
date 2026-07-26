// Package manifest defines the modelmove model manifest: the content-addressed
// description of a model directory that both sides of a transfer agree on.
package manifest

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/shaneburrell/modelmove/internal/chunk"
	"github.com/shaneburrell/modelmove/internal/layout"
)

// Format and Version identify the manifest schema.
const (
	Format  = "modelmove-manifest"
	Version = 1
)

// StateDir is the per-destination directory holding the applied manifest,
// staging files and the resume journal. It is always excluded from transfers.
const StateDir = ".modelmove"

// ManifestName is the manifest written into StateDir after a successful apply.
const ManifestName = "manifest.json"

// Chunker records the parameters needed to reproduce chunk boundaries.
type Chunker struct {
	Algorithm string `json:"algorithm"`
	AvgSize   uint32 `json:"avg_size"`
	MinSize   uint32 `json:"min_size"`
	MaxSize   uint32 `json:"max_size"`
}

// Options converts to chunker options.
func (c Chunker) Options() chunk.Options {
	return chunk.Options{AvgSize: c.AvgSize, MinSize: c.MinSize, MaxSize: c.MaxSize}
}

// ChunkerFrom builds a Chunker from chunker options.
func ChunkerFrom(o chunk.Options) Chunker {
	return Chunker{Algorithm: chunk.ChunkerAlgorithm, AvgSize: o.AvgSize, MinSize: o.MinSize, MaxSize: o.MaxSize}
}

// Chunk is one content-defined chunk of one file.
type Chunk struct {
	Offset uint64       `json:"offset"`
	Length uint32       `json:"length"`
	Digest chunk.Digest `json:"digest"`
}

// End returns the offset one past the last byte of the chunk.
func (c Chunk) End() uint64 { return c.Offset + uint64(c.Length) }

// File is one file in the model directory.
type File struct {
	Path    string       `json:"path"`
	Size    int64        `json:"size"`
	Mode    uint32       `json:"mode"`
	ModTime time.Time    `json:"mod_time"`
	Role    layout.Role  `json:"role"`
	Digest  chunk.Digest `json:"digest"`
	Chunker Chunker      `json:"chunker"`
	Chunks  []Chunk      `json:"chunks"`
}

// Options returns the chunker options for this file.
func (f *File) Options() chunk.Options { return f.Chunker.Options() }

// Model carries the directory-level summary.
type Model struct {
	Name        string            `json:"name,omitempty"`
	Kind        layout.Kind       `json:"kind"`
	Root        string            `json:"root,omitempty"`
	Files       int               `json:"files"`
	Size        int64             `json:"size"`
	WeightFiles int               `json:"weight_files"`
	WeightBytes int64             `json:"weight_bytes"`
	Shards      []layout.ShardSet `json:"shards,omitempty"`
	Digest      chunk.Digest      `json:"digest"`
	Extra       map[string]string `json:"extra,omitempty"`
}

// Manifest is the full description of a model directory.
type Manifest struct {
	Format    string    `json:"format"`
	Version   int       `json:"version"`
	Tool      string    `json:"tool,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Hash      string    `json:"hash"`
	Chunker   Chunker   `json:"chunker"`
	Model     Model     `json:"model"`
	Files     []*File   `json:"files"`
}

// New returns an empty manifest with the schema fields filled in.
func New(tool string, defaults chunk.Options) *Manifest {
	return &Manifest{
		Format:    Format,
		Version:   Version,
		Tool:      tool,
		CreatedAt: time.Now().UTC().Truncate(time.Second),
		Hash:      chunk.HashAlgorithm,
		Chunker:   ChunkerFrom(defaults.Normalized()),
	}
}

// Sort orders files by path so that two manifests of the same tree compare
// equal regardless of directory iteration order.
func (m *Manifest) Sort() {
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
}

// Lookup returns the file entry for a relative path.
func (m *Manifest) Lookup(rel string) *File {
	for _, f := range m.Files {
		if f.Path == rel {
			return f
		}
	}
	return nil
}

// TotalBytes returns the sum of all file sizes.
func (m *Manifest) TotalBytes() int64 {
	var n int64
	for _, f := range m.Files {
		n += f.Size
	}
	return n
}

// TotalChunks returns the number of chunk entries across all files.
func (m *Manifest) TotalChunks() int {
	n := 0
	for _, f := range m.Files {
		n += len(f.Chunks)
	}
	return n
}

// UniqueChunks returns the count and byte total of distinct chunk digests,
// which is the real transfer cost of the model on an empty destination.
func (m *Manifest) UniqueChunks() (count int, bytes int64) {
	seen := make(map[chunk.Digest]struct{})
	for _, f := range m.Files {
		for _, c := range f.Chunks {
			if _, ok := seen[c.Digest]; ok {
				continue
			}
			seen[c.Digest] = struct{}{}
			count++
			bytes += int64(c.Length)
		}
	}
	return count, bytes
}

// DigestSet returns the distinct chunk digests mapped to their lengths.
func (m *Manifest) DigestSet() map[chunk.Digest]uint32 {
	out := make(map[chunk.Digest]uint32)
	for _, f := range m.Files {
		for _, c := range f.Chunks {
			out[c.Digest] = c.Length
		}
	}
	return out
}

// Paths returns the relative paths of every file, in manifest order.
func (m *Manifest) Paths() []string {
	out := make([]string, len(m.Files))
	for i, f := range m.Files {
		out[i] = f.Path
	}
	return out
}

// Finalize sorts the files, recomputes the model summary and derives the
// model-level digest from the per-file digests.
func (m *Manifest) Finalize(info layout.Info) {
	m.Sort()
	m.Model.Kind = info.Kind
	if m.Model.Name == "" {
		m.Model.Name = info.Name
	}
	m.Model.Shards = info.Shards
	m.Model.Files = len(m.Files)
	m.Model.Size = 0
	m.Model.WeightFiles = 0
	m.Model.WeightBytes = 0

	h := chunk.NewHasher()
	for _, f := range m.Files {
		m.Model.Size += f.Size
		if f.Role == layout.RoleWeight {
			m.Model.WeightFiles++
			m.Model.WeightBytes += f.Size
		}
		fmt.Fprintf(h, "%s\x00%d\x00%s\n", f.Path, f.Size, f.Digest)
	}
	var d chunk.Digest
	copy(d[:], h.Sum(nil))
	m.Model.Digest = d
}

// Recount recomputes the derived model totals from the file list. The binary
// encoding leaves them out because they are redundant, so a decoder has to put
// them back.
func (m *Manifest) Recount() {
	m.Model.Files = len(m.Files)
	m.Model.Size = 0
	m.Model.WeightFiles = 0
	m.Model.WeightBytes = 0
	for _, f := range m.Files {
		m.Model.Size += f.Size
		if f.Role == layout.RoleWeight {
			m.Model.WeightFiles++
			m.Model.WeightBytes += f.Size
		}
	}
}

// Validate checks the structural invariants a manifest must satisfy before it
// is trusted: contiguous chunk coverage, matching totals and a known schema.
func (m *Manifest) Validate() error {
	if m.Format != Format {
		return fmt.Errorf("manifest: unknown format %q", m.Format)
	}
	if m.Version != Version {
		return fmt.Errorf("manifest: unsupported version %d", m.Version)
	}
	if m.Hash != chunk.HashAlgorithm {
		return fmt.Errorf("manifest: unsupported hash %q", m.Hash)
	}
	seen := make(map[string]struct{}, len(m.Files))
	for _, f := range m.Files {
		if err := validateFile(f); err != nil {
			return err
		}
		if _, dup := seen[f.Path]; dup {
			return fmt.Errorf("manifest: duplicate path %q", f.Path)
		}
		seen[f.Path] = struct{}{}
	}
	return nil
}

func validateFile(f *File) error {
	if f == nil {
		return fmt.Errorf("manifest: nil file entry")
	}
	if err := ValidatePath(f.Path); err != nil {
		return err
	}
	if f.Size < 0 {
		return fmt.Errorf("manifest: %s: negative size", f.Path)
	}
	if f.Chunker.Algorithm != "" && f.Chunker.Algorithm != chunk.ChunkerAlgorithm {
		return fmt.Errorf("manifest: %s: unsupported chunker %q", f.Path, f.Chunker.Algorithm)
	}
	var off uint64
	for i, c := range f.Chunks {
		if c.Offset != off {
			return fmt.Errorf("manifest: %s: chunk %d starts at %d, expected %d", f.Path, i, c.Offset, off)
		}
		if c.Length == 0 {
			return fmt.Errorf("manifest: %s: chunk %d has zero length", f.Path, i)
		}
		off += uint64(c.Length)
	}
	if int64(off) != f.Size {
		return fmt.Errorf("manifest: %s: chunks cover %d bytes, file is %d", f.Path, off, f.Size)
	}
	return nil
}

// ValidatePath rejects paths that would escape the destination root. Manifests
// arrive over the network, so this is a security boundary, not a nicety.
func ValidatePath(rel string) error {
	switch {
	case rel == "":
		return fmt.Errorf("manifest: empty path")
	case path.IsAbs(rel) || strings.HasPrefix(rel, "/"):
		return fmt.Errorf("manifest: absolute path %q", rel)
	case strings.Contains(rel, `\`):
		return fmt.Errorf("manifest: backslash in path %q", rel)
	case rel != path.Clean(rel):
		return fmt.Errorf("manifest: unclean path %q", rel)
	case rel == ".." || strings.HasPrefix(rel, "../"):
		return fmt.Errorf("manifest: path escapes root: %q", rel)
	case rel == StateDir || strings.HasPrefix(rel, StateDir+"/"):
		return fmt.Errorf("manifest: path inside %s: %q", StateDir, rel)
	case strings.ContainsRune(rel, 0):
		return fmt.Errorf("manifest: NUL in path %q", rel)
	}
	return nil
}
