package manifest

import (
	"sort"

	"github.com/shaneburrell/modelmove/internal/chunk"
)

// FileChange classifies how one path differs between two manifests.
type FileChange string

// File change classes.
const (
	ChangeAdded     FileChange = "added"
	ChangeRemoved   FileChange = "removed"
	ChangeModified  FileChange = "modified"
	ChangeUnchanged FileChange = "unchanged"
)

// FileDiff describes the difference for a single path.
type FileDiff struct {
	Path        string     `json:"path"`
	Change      FileChange `json:"change"`
	OldSize     int64      `json:"old_size,omitempty"`
	NewSize     int64      `json:"new_size,omitempty"`
	SharedBytes int64      `json:"shared_bytes,omitempty"`
	NewBytes    int64      `json:"new_bytes,omitempty"`
	NewChunks   int        `json:"new_chunks,omitempty"`
}

// Diff summarises the delta between two manifests, which is the same
// computation a transfer performs: how many bytes actually have to move.
type Diff struct {
	Files        []FileDiff `json:"files"`
	Added        int        `json:"added"`
	Removed      int        `json:"removed"`
	Modified     int        `json:"modified"`
	Unchanged    int        `json:"unchanged"`
	OldBytes     int64      `json:"old_bytes"`
	NewBytes     int64      `json:"new_bytes"`
	ReusableByte int64      `json:"reusable_bytes"`
	TransferByte int64      `json:"transfer_bytes"`
	Identical    bool       `json:"identical"`
}

// Similarity returns the fraction of the new manifest that already exists in
// the old one, between 0 and 1.
func (d Diff) Similarity() float64 {
	if d.NewBytes == 0 {
		return 1
	}
	return float64(d.ReusableByte) / float64(d.NewBytes)
}

// Compare computes the delta from old to want. Chunk reuse is global: a chunk
// present anywhere in old counts as available, which is what lets a renamed or
// re-sharded checkpoint transfer almost nothing.
func Compare(old, want *Manifest) Diff {
	var d Diff
	have := map[chunk.Digest]struct{}{}
	if old != nil {
		for _, f := range old.Files {
			for _, c := range f.Chunks {
				have[c.Digest] = struct{}{}
			}
			d.OldBytes += f.Size
		}
	}

	oldByPath := map[string]*File{}
	if old != nil {
		for _, f := range old.Files {
			oldByPath[f.Path] = f
		}
	}

	// Track digests already counted as transferred so that a chunk shared by
	// two new files is only paid for once.
	sent := map[chunk.Digest]struct{}{}

	for _, f := range want.Files {
		d.NewBytes += f.Size
		fd := FileDiff{Path: f.Path, NewSize: f.Size}

		prev, existed := oldByPath[f.Path]
		switch {
		case !existed:
			fd.Change = ChangeAdded
			d.Added++
		case prev.Digest == f.Digest && prev.Size == f.Size:
			fd.Change = ChangeUnchanged
			fd.OldSize = prev.Size
			d.Unchanged++
		default:
			fd.Change = ChangeModified
			fd.OldSize = prev.Size
			d.Modified++
		}

		for _, c := range f.Chunks {
			_, known := have[c.Digest]
			if known {
				fd.SharedBytes += int64(c.Length)
				continue
			}
			if _, dup := sent[c.Digest]; dup {
				fd.SharedBytes += int64(c.Length)
				continue
			}
			sent[c.Digest] = struct{}{}
			fd.NewBytes += int64(c.Length)
			fd.NewChunks++
		}
		d.ReusableByte += fd.SharedBytes
		d.TransferByte += fd.NewBytes
		d.Files = append(d.Files, fd)
	}

	if old != nil {
		wantPaths := map[string]struct{}{}
		for _, f := range want.Files {
			wantPaths[f.Path] = struct{}{}
		}
		for _, f := range old.Files {
			if _, ok := wantPaths[f.Path]; !ok {
				d.Files = append(d.Files, FileDiff{Path: f.Path, Change: ChangeRemoved, OldSize: f.Size})
				d.Removed++
			}
		}
	}

	sort.Slice(d.Files, func(i, j int) bool { return d.Files[i].Path < d.Files[j].Path })
	d.Identical = d.Added == 0 && d.Removed == 0 && d.Modified == 0
	return d
}
