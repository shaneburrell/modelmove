package manifest

import (
	"testing"

	"github.com/shaneburrell/modelmove/internal/chunk"
	"github.com/shaneburrell/modelmove/internal/layout"
)

func fileWith(path string, digests ...byte) *File {
	f := &File{
		Path:    path,
		Role:    layout.RoleWeight,
		Chunker: ChunkerFrom(chunk.LargeFileOptions()),
	}
	var off uint64
	h := chunk.NewHasher()
	for _, d := range digests {
		cd := digest(d)
		f.Chunks = append(f.Chunks, Chunk{Offset: off, Length: 100, Digest: cd})
		_, _ = h.Write(cd[:])
		off += 100
	}
	f.Size = int64(off)
	copy(f.Digest[:], h.Sum(nil))
	return f
}

func manifestWith(files ...*File) *Manifest {
	m := New("t", chunk.LargeFileOptions())
	m.Files = files
	m.Recount()
	return m
}

func TestCompareIdentical(t *testing.T) {
	a := manifestWith(fileWith("w.safetensors", 1, 2, 3))
	b := manifestWith(fileWith("w.safetensors", 1, 2, 3))
	d := Compare(a, b)

	if !d.Identical {
		t.Fatal("identical manifests reported as different")
	}
	if d.TransferByte != 0 {
		t.Errorf("TransferByte = %d, want 0", d.TransferByte)
	}
	if d.Unchanged != 1 {
		t.Errorf("Unchanged = %d, want 1", d.Unchanged)
	}
	if s := d.Similarity(); s != 1 {
		t.Errorf("Similarity = %v, want 1", s)
	}
}

func TestComparePartialChange(t *testing.T) {
	a := manifestWith(fileWith("w.safetensors", 1, 2, 3, 4))
	b := manifestWith(fileWith("w.safetensors", 1, 9, 3, 4))
	d := Compare(a, b)

	if d.Identical {
		t.Fatal("changed manifests reported as identical")
	}
	if d.Modified != 1 {
		t.Errorf("Modified = %d, want 1", d.Modified)
	}
	// Only the one new chunk moves.
	if d.TransferByte != 100 {
		t.Errorf("TransferByte = %d, want 100", d.TransferByte)
	}
	if d.ReusableByte != 300 {
		t.Errorf("ReusableByte = %d, want 300", d.ReusableByte)
	}
	if got := d.Similarity(); got < 0.74 || got > 0.76 {
		t.Errorf("Similarity = %v, want ~0.75", got)
	}
}

func TestCompareAddedAndRemoved(t *testing.T) {
	a := manifestWith(fileWith("old.safetensors", 1, 2))
	b := manifestWith(fileWith("new.safetensors", 3, 4))
	d := Compare(a, b)

	if d.Added != 1 || d.Removed != 1 {
		t.Errorf("Added/Removed = %d/%d, want 1/1", d.Added, d.Removed)
	}
	if len(d.Files) != 2 {
		t.Fatalf("got %d file diffs, want 2", len(d.Files))
	}
	if d.Files[0].Path != "new.safetensors" || d.Files[0].Change != ChangeAdded {
		t.Errorf("first diff = %+v", d.Files[0])
	}
	if d.Files[1].Change != ChangeRemoved {
		t.Errorf("second diff = %+v", d.Files[1])
	}
}

// TestCompareRenameIsFree is the point of content addressing: moving a file
// costs nothing because the chunks already exist.
func TestCompareRenameIsFree(t *testing.T) {
	a := manifestWith(fileWith("model-00001-of-00002.safetensors", 1, 2, 3))
	b := manifestWith(fileWith("model-00001-of-00003.safetensors", 1, 2, 3))
	d := Compare(a, b)

	if d.TransferByte != 0 {
		t.Errorf("a pure rename wants %d bytes, should want 0", d.TransferByte)
	}
	if d.Added != 1 || d.Removed != 1 {
		t.Errorf("Added/Removed = %d/%d", d.Added, d.Removed)
	}
}

// TestCompareDeduplicatesAcrossNewFiles checks that a chunk shared by two new
// files is only counted once.
func TestCompareDeduplicatesAcrossNewFiles(t *testing.T) {
	b := manifestWith(
		fileWith("a.safetensors", 1, 2),
		fileWith("b.safetensors", 1, 2),
	)
	d := Compare(nil, b)
	if d.TransferByte != 200 {
		t.Errorf("TransferByte = %d, want 200 (the duplicate pays nothing)", d.TransferByte)
	}
	if d.NewBytes != 400 {
		t.Errorf("NewBytes = %d, want 400", d.NewBytes)
	}
}

func TestCompareAgainstNothing(t *testing.T) {
	b := manifestWith(fileWith("w.safetensors", 1, 2))
	d := Compare(nil, b)
	if d.Added != 1 || d.TransferByte != 200 {
		t.Errorf("diff against nil = %+v", d)
	}
	if d.Identical {
		t.Error("a fresh model is not identical to nothing")
	}
}

func TestSimilarityWithEmptyManifest(t *testing.T) {
	d := Compare(nil, New("t", chunk.SmallFileOptions()))
	if d.Similarity() != 1 {
		t.Errorf("Similarity of an empty manifest = %v, want 1", d.Similarity())
	}
	if !d.Identical {
		t.Error("two empty manifests are identical")
	}
}
