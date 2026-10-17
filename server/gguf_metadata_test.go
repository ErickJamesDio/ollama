package server

import (
	"math"
	"os"
	"slices"
	"testing"

	"github.com/ollama/ollama/fs/ggml"
	"github.com/ollama/ollama/manifest"
	"github.com/ollama/ollama/types/model"
)

func TestGGUFMetadataExtraction(t *testing.T) {
	t.Setenv("OLLAMA_MODELS", t.TempDir())

	largeTokenizer := make([]string, ggufMetadataMaxArray+1)
	for i := range largeTokenizer {
		largeTokenizer[i] = "token"
	}

	path, _ := createBinFile(t, ggml.KV{
		"general.architecture":     "qwen35",
		"qwen35.context_length":    uint32(32768),
		"qwen35.attention.softcap": float32(math.Inf(-1)),
		"tokenizer.chat_template":  "{{ messages }}",
		"tokenizer.ggml.tokens":    largeTokenizer,
	}, nil)

	metadata, err := extractGGUFMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := metadata.String("general.architecture"); got != "qwen35" {
		t.Fatalf("architecture = %q, want qwen35", got)
	}
	if got := metadata.Int("context_length"); got != 32768 {
		t.Fatalf("context_length = %d, want 32768", got)
	}
	if got := metadata.String("tokenizer.chat_template"); got != "{{ messages }}" {
		t.Fatalf("chat template = %q", got)
	}
	if !slices.Contains(metadata.Omitted, "tokenizer.ggml.tokens") {
		t.Fatalf("large tokenizer was not marked omitted: %v", metadata.Omitted)
	}
	if !slices.Contains(metadata.Omitted, "qwen35.attention.softcap") {
		t.Fatalf("non-finite value was not marked omitted: %v", metadata.Omitted)
	}
}

func TestGGUFMetadataCacheByDigest(t *testing.T) {
	t.Setenv("OLLAMA_MODELS", t.TempDir())

	_, digest := createBinFile(t, ggml.KV{
		"general.architecture": "qwen35",
		"qwen35.block_count":   uint32(33),
	}, nil)

	first, err := readGGUFMetadata(digest)
	if err != nil {
		t.Fatal(err)
	}
	if got := first.Int("block_count"); got != 33 {
		t.Fatalf("block_count = %d, want 33", got)
	}

	blobPath, err := manifest.BlobsPath(digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blobPath); err != nil {
		t.Fatal(err)
	}

	second, err := readGGUFMetadata(digest)
	if err != nil {
		t.Fatalf("metadata cache was not used: %v", err)
	}
	if got := second.Int("block_count"); got != 33 {
		t.Fatalf("cached block_count = %d, want 33", got)
	}
}

func TestProjectorMetadataPreservesVisionCapability(t *testing.T) {
	m := &Model{
		ModelPath:      "model.gguf",
		ProjectorPaths: []string{"mmproj.gguf"},
		Config:         model.ConfigV2{ModelFormat: "gguf", ModelFamily: "qwen35"},
		metadata: ggufMetadata{KV: map[string]any{
			"general.architecture": "qwen35",
		}},
		projectorMetadata: []ggufMetadata{{KV: map[string]any{
			"general.architecture": "clip",
		}}},
	}

	capabilities := m.Capabilities()
	if !slices.Contains(capabilities, model.CapabilityCompletion) {
		t.Fatalf("completion capability missing: %v", capabilities)
	}
	if !slices.Contains(capabilities, model.CapabilityVision) {
		t.Fatalf("vision capability missing: %v", capabilities)
	}
}
