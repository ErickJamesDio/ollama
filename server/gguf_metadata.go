package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/ollama/ollama/envconfig"
	fsgguf "github.com/ollama/ollama/fs/gguf"
	"github.com/ollama/ollama/manifest"
	"github.com/ollama/ollama/version"
)

const ggufMetadataMaxArray = 4096

// ggufMetadata is a compact, content-addressed snapshot of a blob's metadata.
// Large tokenizer arrays are recorded as omitted rather than copied.
type ggufMetadata struct {
	OllamaVersion string         `json:"ollama_version,omitempty"`
	KV            map[string]any `json:"kv"`
	Omitted       []string       `json:"omitted,omitempty"`
}

func (m ggufMetadata) lookup(key string) (any, bool) {
	if !strings.HasPrefix(key, "general.") && !strings.HasPrefix(key, "tokenizer.") {
		arch, _ := m.KV["general.architecture"].(string)
		key = arch + "." + key
	}
	v, ok := m.KV[key]
	return v, ok
}

func (m ggufMetadata) Valid(key string) bool {
	_, ok := m.lookup(key)
	return ok
}

func (m ggufMetadata) String(key string) string {
	v, _ := m.lookup(key)
	s, _ := v.(string)
	return s
}

func (m ggufMetadata) Int(key string) int64 {
	v, _ := m.lookup(key)
	n, ok := v.(json.Number)
	if !ok {
		return 0
	}
	i, _ := n.Int64()
	return i
}

func (m ggufMetadata) Keys() []string {
	keys := make([]string, 0, len(m.KV)+len(m.Omitted))
	for key := range m.KV {
		keys = append(keys, key)
	}
	return append(keys, m.Omitted...)
}

func ggufMetadataPath(digest string) (string, error) {
	if err := manifest.ValidateDigest(digest); err != nil {
		return "", fmt.Errorf("%w: %q", err, digest)
	}
	return filepath.Join(envconfig.Models(), "metadata", strings.ReplaceAll(digest, ":", "-")+".json"), nil
}

func readGGUFMetadata(digest string) (ggufMetadata, error) {
	path, err := ggufMetadataPath(digest)
	if err != nil {
		return ggufMetadata{}, err
	}
	if md, ok := loadGGUFMetadata(path); ok {
		return md, nil
	}

	blob, err := manifest.BlobsPath(digest)
	if err != nil {
		return ggufMetadata{}, err
	}
	md, err := extractGGUFMetadata(blob)
	if err != nil {
		return ggufMetadata{}, err
	}
	writeGGUFMetadata(path, md)
	return md, nil
}

func loadGGUFMetadata(path string) (ggufMetadata, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ggufMetadata{}, false
	}
	md, err := decodeGGUFMetadata(data)
	if err != nil {
		slog.Debug("ignoring unusable gguf metadata", "path", path, "error", err)
		return ggufMetadata{}, false
	}
	return md, true
}

func writeGGUFMetadata(path string, md ggufMetadata) {
	data, err := json.Marshal(md)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Debug("could not create gguf metadata directory", "path", path, "error", err)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gguf-metadata-*.tmp")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		slog.Debug("could not persist gguf metadata", "path", path, "error", err)
	}
}

func extractGGUFMetadata(path string) (ggufMetadata, error) {
	f, err := fsgguf.Open(path)
	if err != nil {
		return ggufMetadata{}, err
	}
	defer f.Close()

	md := ggufMetadata{OllamaVersion: version.Version, KV: make(map[string]any)}
	for _, kv := range f.KeyValues() {
		if omitGGUFMetadataValue(kv.Any()) {
			md.Omitted = append(md.Omitted, kv.Key)
			continue
		}
		md.KV[kv.Key] = kv.Any()
	}
	if err := f.Err(); err != nil {
		return ggufMetadata{}, err
	}

	data, err := json.Marshal(md)
	if err != nil {
		return ggufMetadata{}, err
	}
	return decodeGGUFMetadata(data)
}

func decodeGGUFMetadata(data []byte) (ggufMetadata, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var md ggufMetadata
	if err := dec.Decode(&md); err != nil {
		return ggufMetadata{}, err
	}
	if md.KV == nil {
		return ggufMetadata{}, errors.New("no metadata")
	}
	return md, nil
}

func omitGGUFMetadataValue(value any) bool {
	switch v := value.(type) {
	case float32:
		return math.IsInf(float64(v), 0) || math.IsNaN(float64(v))
	case float64:
		return math.IsInf(v, 0) || math.IsNaN(v)
	case []string:
		return len(v) > ggufMetadataMaxArray
	case []int8:
		return len(v) > ggufMetadataMaxArray
	case []int16:
		return len(v) > ggufMetadataMaxArray
	case []int32:
		return len(v) > ggufMetadataMaxArray
	case []int64:
		return len(v) > ggufMetadataMaxArray
	case []uint8:
		return len(v) > ggufMetadataMaxArray
	case []uint16:
		return len(v) > ggufMetadataMaxArray
	case []uint32:
		return len(v) > ggufMetadataMaxArray
	case []uint64:
		return len(v) > ggufMetadataMaxArray
	case []float32:
		if len(v) > ggufMetadataMaxArray {
			return true
		}
		for _, n := range v {
			if math.IsInf(float64(n), 0) || math.IsNaN(float64(n)) {
				return true
			}
		}
	case []float64:
		if len(v) > ggufMetadataMaxArray {
			return true
		}
		for _, n := range v {
			if math.IsInf(n, 0) || math.IsNaN(n) {
				return true
			}
		}
	case []bool:
		return len(v) > ggufMetadataMaxArray
	}
	return false
}
