package layout

import (
	"strings"
	"testing"
)

const gib = 1 << 30

func TestDetectHuggingFaceSharded(t *testing.T) {
	files := []File{
		{Path: "config.json", Size: 800},
		{Path: "generation_config.json", Size: 120},
		{Path: "model.safetensors.index.json", Size: 30000},
		{Path: "model-00001-of-00003.safetensors", Size: 5 * gib},
		{Path: "model-00002-of-00003.safetensors", Size: 5 * gib},
		{Path: "model-00003-of-00003.safetensors", Size: 3 * gib},
		{Path: "tokenizer.json", Size: 2 << 20},
		{Path: "tokenizer_config.json", Size: 900},
		{Path: "README.md", Size: 4000},
	}
	info := Detect("/models/llama-3-8b", files)

	if info.Kind != KindHuggingFace {
		t.Fatalf("Kind = %q, want huggingface", info.Kind)
	}
	if info.Name != "llama-3-8b" {
		t.Errorf("Name = %q", info.Name)
	}
	if info.WeightFiles != 3 || info.WeightBytes != 13*gib {
		t.Errorf("weights = %d files / %d bytes", info.WeightFiles, info.WeightBytes)
	}
	if info.TotalFiles != len(files) {
		t.Errorf("TotalFiles = %d, want %d", info.TotalFiles, len(files))
	}

	roles := map[string]Role{
		"config.json":                      RoleConfig,
		"generation_config.json":           RoleConfig,
		"model.safetensors.index.json":     RoleIndex,
		"model-00001-of-00003.safetensors": RoleWeight,
		"tokenizer.json":                   RoleTokenizer,
		"tokenizer_config.json":            RoleTokenizer,
		"README.md":                        RoleMetadata,
	}
	for path, want := range roles {
		if got := info.RoleOf(path); got != want {
			t.Errorf("role of %s = %q, want %q", path, got, want)
		}
	}
	if got := info.RoleOf("nope"); got != RoleOther {
		t.Errorf("unknown path role = %q", got)
	}

	if len(info.Shards) != 1 {
		t.Fatalf("found %d shard sets, want 1", len(info.Shards))
	}
	s := info.Shards[0]
	if s.Total != 3 || len(s.Parts) != 3 || !s.Complete() {
		t.Errorf("shard set = %+v", s)
	}
	if s.Size != 13*gib {
		t.Errorf("shard set size = %d", s.Size)
	}
	if !strings.Contains(info.Describe(), "huggingface") {
		t.Errorf("Describe() = %q", info.Describe())
	}
}

func TestDetectIncompleteShardSet(t *testing.T) {
	info := Detect("m", []File{
		{Path: "model-00001-of-00004.safetensors", Size: gib},
		{Path: "model-00002-of-00004.safetensors", Size: gib},
	})
	if len(info.Shards) != 1 {
		t.Fatalf("found %d shard sets", len(info.Shards))
	}
	if info.Shards[0].Complete() {
		t.Error("a 2-of-4 shard set should not report complete")
	}
}

func TestDetectGGUF(t *testing.T) {
	info := Detect("/models/mistral-gguf", []File{
		{Path: "mistral-7b-q4_k_m.gguf", Size: 4 * gib},
		{Path: "Modelfile", Size: 200},
	})
	if info.Kind != KindGGUF {
		t.Fatalf("Kind = %q, want gguf", info.Kind)
	}
	if info.RoleOf("mistral-7b-q4_k_m.gguf") != RoleWeight {
		t.Error("the gguf file should be a weight")
	}
	if info.RoleOf("Modelfile") != RoleMetadata {
		t.Error("Modelfile should be metadata")
	}
}

func TestDetectGGUFSplit(t *testing.T) {
	info := Detect("m", []File{
		{Path: "llama-70b-00001-of-00002.gguf", Size: 20 * gib},
		{Path: "llama-70b-00002-of-00002.gguf", Size: 20 * gib},
	})
	if info.Kind != KindGGUF {
		t.Fatalf("Kind = %q", info.Kind)
	}
	if len(info.Shards) != 1 || info.Shards[0].Total != 2 {
		t.Fatalf("shard sets = %+v", info.Shards)
	}
}

func TestDetectOllama(t *testing.T) {
	blob := "blobs/sha256-" + strings.Repeat("a", 64)
	small := "blobs/sha256-" + strings.Repeat("b", 64)
	info := Detect("/root/.ollama/models", []File{
		{Path: blob, Size: 4 * gib},
		{Path: small, Size: 512},
		{Path: "manifests/registry.ollama.ai/library/llama3/latest", Size: 700},
	})
	if info.Kind != KindOllama {
		t.Fatalf("Kind = %q, want ollama", info.Kind)
	}
	if info.RoleOf(blob) != RoleWeight {
		t.Error("a large blob should be a weight")
	}
	if info.RoleOf(small) != RoleMetadata {
		t.Error("a small blob should be metadata")
	}
	if info.WeightFiles != 1 {
		t.Errorf("WeightFiles = %d, want 1", info.WeightFiles)
	}
}

func TestDetectGeneric(t *testing.T) {
	info := Detect("stuff", []File{
		{Path: "notes.rst", Size: 10},
		{Path: "data.csv", Size: 20},
	})
	if info.Kind != KindGeneric {
		t.Fatalf("Kind = %q, want generic", info.Kind)
	}
	if info.RoleOf("data.csv") != RoleOther {
		t.Errorf("csv role = %q", info.RoleOf("data.csv"))
	}
}

func TestDisplayNameFromHuggingFaceCache(t *testing.T) {
	info := Detect("/home/u/.cache/huggingface/hub/models--meta-llama--Llama-3-8B", []File{
		{Path: "config.json", Size: 1},
		{Path: "model.safetensors", Size: 2},
	})
	if info.Name != "meta-llama/Llama-3-8B" {
		t.Errorf("Name = %q", info.Name)
	}
}

func TestDisplayNameEdgeCases(t *testing.T) {
	for _, root := range []string{"/", ".", ""} {
		if got := Detect(root, nil).Name; got != "" {
			t.Errorf("Detect(%q).Name = %q, want empty", root, got)
		}
	}
	if got := Detect("/models/x/", nil).Name; got != "x" {
		t.Errorf("trailing slash produced %q", got)
	}
}

func TestSentencePieceModelIsNotAWeight(t *testing.T) {
	info := Detect("m", []File{
		{Path: "spiece.model", Size: 800 << 10},
		{Path: "big.model", Size: 2 * gib},
	})
	if info.RoleOf("spiece.model") != RoleTokenizer {
		t.Error("a small .model file should be a tokenizer")
	}
	if info.RoleOf("big.model") != RoleWeight {
		t.Error("a multi-gigabyte .model file should be a weight")
	}
}

func TestTransferOrder(t *testing.T) {
	files := []File{
		{Path: "model-00001-of-00002.safetensors", Size: gib},
		{Path: "tokenizer.json", Size: 100},
		{Path: "config.json", Size: 50},
		{Path: "model.safetensors.index.json", Size: 60},
		{Path: "README.md", Size: 10},
	}
	info := Detect("m", files)
	order := TransferOrder(files, info.Roles)

	if order[0] != "config.json" {
		t.Errorf("config should come first, got %q", order[0])
	}
	if order[len(order)-1] != "model-00001-of-00002.safetensors" {
		t.Errorf("weights should come last, got %q", order[len(order)-1])
	}
	if len(order) != len(files) {
		t.Fatalf("TransferOrder returned %d paths for %d files", len(order), len(files))
	}
}
