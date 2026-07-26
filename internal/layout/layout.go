// Package layout recognises the on-disk conventions used by the common model
// distribution formats, so that modelmove can treat a sharded multi-gigabyte
// checkpoint as one logical object rather than a bag of files.
package layout

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Kind identifies a recognised directory convention.
type Kind string

// Recognised directory conventions.
const (
	KindHuggingFace Kind = "huggingface"
	KindGGUF        Kind = "gguf"
	KindOllama      Kind = "ollama"
	KindGeneric     Kind = "generic"
)

// Role classifies a single file within a model directory. Roles drive chunker
// sizing and the ordering of the transfer: metadata is small and cheap, weight
// shards are where all the bytes are.
type Role string

// File roles.
const (
	RoleWeight    Role = "weight"
	RoleIndex     Role = "index"
	RoleConfig    Role = "config"
	RoleTokenizer Role = "tokenizer"
	RoleMetadata  Role = "metadata"
	RoleOther     Role = "other"
)

// File is the minimal input Detect needs about a directory entry.
type File struct {
	Path string // slash-separated path relative to the model root
	Size int64
}

// ShardSet groups the parts of one logically single weight file.
type ShardSet struct {
	Name  string   `json:"name"`
	Parts []string `json:"parts"`
	Total int      `json:"total"`
	Size  int64    `json:"size"`
}

// Complete reports whether every declared part is present.
func (s ShardSet) Complete() bool { return s.Total > 0 && len(s.Parts) == s.Total }

// Info is the result of inspecting a model directory.
type Info struct {
	Kind        Kind            `json:"kind"`
	Name        string          `json:"name,omitempty"`
	Roles       map[string]Role `json:"-"`
	Shards      []ShardSet      `json:"shards,omitempty"`
	WeightFiles int             `json:"weight_files"`
	WeightBytes int64           `json:"weight_bytes"`
	TotalFiles  int             `json:"total_files"`
	TotalBytes  int64           `json:"total_bytes"`
}

// RoleOf returns the role recorded for a relative path.
func (i Info) RoleOf(rel string) Role {
	if r, ok := i.Roles[rel]; ok {
		return r
	}
	return RoleOther
}

// Describe renders a one-line human summary.
func (i Info) Describe() string {
	var b strings.Builder
	b.WriteString(string(i.Kind))
	if i.Name != "" {
		fmt.Fprintf(&b, " %q", i.Name)
	}
	fmt.Fprintf(&b, ", %d files, %d weight file(s)", i.TotalFiles, i.WeightFiles)
	if n := len(i.Shards); n > 0 {
		fmt.Fprintf(&b, ", %d shard set(s)", n)
	}
	return b.String()
}

// weightExts are the file extensions that hold tensor data.
var weightExts = map[string]bool{
	".safetensors": true,
	".gguf":        true,
	".bin":         true,
	".pt":          true,
	".pth":         true,
	".ckpt":        true,
	".onnx":        true,
	".msgpack":     true,
	".h5":          true,
	".npz":         true,
	".model":       true,
}

// shardRe matches the "-00001-of-00003" convention used by Hugging Face and
// by llama.cpp GGUF splits.
var shardRe = regexp.MustCompile(`^(.*?)-(\d{2,6})-of-(\d{2,6})(\.[A-Za-z0-9]+)$`)

// ollamaBlobRe matches Ollama's content-addressed blob filenames.
var ollamaBlobRe = regexp.MustCompile(`^sha256[-:][0-9a-f]{64}$`)

// Detect classifies a set of files. The root basename is only used to guess a
// display name and never affects roles.
func Detect(root string, files []File) Info {
	info := Info{
		Kind:  KindGeneric,
		Roles: make(map[string]Role, len(files)),
		Name:  displayName(root),
	}

	var (
		hasConfig    bool
		hasIndex     bool
		hasSafe      bool
		hasGGUF      bool
		ollamaBlobs  int
		ollamaManifs int
	)

	for _, f := range files {
		base := path.Base(f.Path)
		lower := strings.ToLower(base)
		ext := strings.ToLower(path.Ext(base))
		dir := path.Dir(f.Path)

		switch {
		case lower == "config.json":
			hasConfig = true
		case strings.HasSuffix(lower, ".index.json"):
			hasIndex = true
		case ext == ".safetensors":
			hasSafe = true
		case ext == ".gguf":
			hasGGUF = true
		}
		if ollamaBlobRe.MatchString(lower) && strings.HasPrefix(dir+"/", "blobs/") {
			ollamaBlobs++
		}
		if strings.HasPrefix(f.Path, "manifests/") {
			ollamaManifs++
		}

		info.Roles[f.Path] = roleFor(f.Path, f.Size)
		info.TotalFiles++
		info.TotalBytes += f.Size
	}

	switch {
	case ollamaBlobs > 0 && ollamaManifs > 0:
		info.Kind = KindOllama
	case hasGGUF && !hasSafe:
		info.Kind = KindGGUF
	case hasConfig && (hasSafe || hasIndex):
		info.Kind = KindHuggingFace
	case hasSafe || hasIndex:
		info.Kind = KindHuggingFace
	}

	// Ollama blobs have no extension, so their role has to come from the
	// layout rather than the filename.
	if info.Kind == KindOllama {
		for _, f := range files {
			if strings.HasPrefix(f.Path, "blobs/") && ollamaBlobRe.MatchString(strings.ToLower(path.Base(f.Path))) {
				if f.Size >= 1<<20 {
					info.Roles[f.Path] = RoleWeight
				} else {
					info.Roles[f.Path] = RoleMetadata
				}
			}
		}
	}

	for _, f := range files {
		if info.Roles[f.Path] == RoleWeight {
			info.WeightFiles++
			info.WeightBytes += f.Size
		}
	}
	info.Shards = detectShards(files, info.Roles)
	return info
}

func displayName(root string) string {
	base := path.Base(strings.ReplaceAll(strings.TrimRight(root, "/"), "\\", "/"))
	if base == "." || base == "/" || base == "" {
		return ""
	}
	// Hugging Face cache directories look like "models--org--name".
	if rest, ok := strings.CutPrefix(base, "models--"); ok {
		return strings.ReplaceAll(rest, "--", "/")
	}
	return base
}

func roleFor(rel string, size int64) Role {
	base := path.Base(rel)
	lower := strings.ToLower(base)
	ext := strings.ToLower(path.Ext(base))

	switch {
	case strings.HasSuffix(lower, ".index.json"):
		return RoleIndex
	case lower == "config.json", lower == "generation_config.json",
		lower == "preprocessor_config.json", lower == "adapter_config.json",
		lower == "quantize_config.json", lower == "params.json":
		return RoleConfig
	case strings.HasPrefix(lower, "tokenizer"), lower == "vocab.json",
		lower == "merges.txt", lower == "special_tokens_map.json",
		lower == "spiece.model", lower == "vocab.txt":
		return RoleTokenizer
	case ext == ".md", ext == ".txt", ext == ".json", ext == ".yaml", ext == ".yml",
		lower == "license", lower == "modelfile":
		return RoleMetadata
	case weightExts[ext]:
		// A ".model" file is a SentencePiece vocabulary far more often than it
		// is a checkpoint, so keep small ones out of the weight class.
		if ext == ".model" && size < 64<<20 {
			return RoleTokenizer
		}
		return RoleWeight
	default:
		return RoleOther
	}
}

// detectShards groups "-00001-of-00003" families into shard sets.
func detectShards(files []File, roles map[string]Role) []ShardSet {
	type group struct {
		parts []string
		total int
		size  int64
	}
	groups := map[string]*group{}

	for _, f := range files {
		if roles[f.Path] != RoleWeight {
			continue
		}
		dir, base := path.Split(f.Path)
		m := shardRe.FindStringSubmatch(base)
		if m == nil {
			continue
		}
		total, err := strconv.Atoi(m[3])
		if err != nil || total <= 1 {
			continue
		}
		key := dir + m[1] + m[4]
		g := groups[key]
		if g == nil {
			g = &group{total: total}
			groups[key] = g
		}
		g.parts = append(g.parts, f.Path)
		g.size += f.Size
	}

	out := make([]ShardSet, 0, len(groups))
	for name, g := range groups {
		sort.Strings(g.parts)
		out = append(out, ShardSet{Name: name, Parts: g.parts, Total: g.total, Size: g.size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// TransferOrder returns a copy of paths ordered for transfer: metadata first
// so that an interrupted run leaves a directory that is obviously incomplete
// rather than one that looks loadable, then weights largest-last so progress
// estimates settle early.
func TransferOrder(files []File, roles map[string]Role) []string {
	rank := map[Role]int{
		RoleConfig:    0,
		RoleIndex:     1,
		RoleTokenizer: 2,
		RoleMetadata:  3,
		RoleOther:     4,
		RoleWeight:    5,
	}
	out := make([]File, len(files))
	copy(out, files)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank[roles[out[i].Path]], rank[roles[out[j].Path]]
		if ri != rj {
			return ri < rj
		}
		return out[i].Path < out[j].Path
	})
	paths := make([]string, len(out))
	for i, f := range out {
		paths[i] = f.Path
	}
	return paths
}
