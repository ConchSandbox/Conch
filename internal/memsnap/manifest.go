// Package memsnap defines the on-disk format for incremental guest memory.
package memsnap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
)

const (
	SchemaVersion    = 1
	ManifestFileName = "manifest.json"
	LayerDirName     = "layers"
	DefaultBlockSize = uint64(4096)
)

type BuildRange struct {
	Offset     uint64 `json:"offset"`
	Length     uint64 `json:"length"`
	LayerIndex int    `json:"layer_index"`
}

type Manifest struct {
	SchemaVersion int          `json:"schema_version"`
	MemorySize    uint64       `json:"memory_size"`
	BlockSize     uint64       `json:"block_size"`
	Layers        []string     `json:"layers"`
	BuildMap      []BuildRange `json:"build_map"`
}

func validateManifest(manifest Manifest) error {
	if manifest.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported manifest schema version %d", manifest.SchemaVersion)
	}
	if manifest.MemorySize == 0 || manifest.BlockSize == 0 || manifest.MemorySize%manifest.BlockSize != 0 {
		return fmt.Errorf("invalid memory geometry")
	}
	if len(manifest.Layers) == 0 {
		return fmt.Errorf("manifest has no layers")
	}
	seen := make(map[string]struct{}, len(manifest.Layers))
	for index, path := range manifest.Layers {
		want := filepath.ToSlash(filepath.Join(LayerDirName, fmt.Sprintf("%d.mem", index)))
		if path != want || filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("layer %d path is %q, want %q", index, path, want)
		}
		if _, exists := seen[path]; exists {
			return fmt.Errorf("duplicate layer path %q", path)
		}
		seen[path] = struct{}{}
	}
	if len(manifest.BuildMap) == 0 {
		return fmt.Errorf("manifest has no build map")
	}
	next := uint64(0)
	previousLayer := -1
	for _, span := range manifest.BuildMap {
		if span.Length == 0 || span.Offset%manifest.BlockSize != 0 || span.Length%manifest.BlockSize != 0 {
			return fmt.Errorf("invalid build range geometry")
		}
		if span.Offset != next || span.Offset >= manifest.MemorySize || span.Length > manifest.MemorySize-span.Offset {
			return fmt.Errorf("build map is not a contiguous cover of memory")
		}
		if span.LayerIndex < 0 || span.LayerIndex >= len(manifest.Layers) {
			return fmt.Errorf("build range names undeclared layer index %d", span.LayerIndex)
		}
		if span.LayerIndex == previousLayer {
			return fmt.Errorf("adjacent build ranges for layer %d are not merged", span.LayerIndex)
		}
		next = span.Offset + span.Length
		previousLayer = span.LayerIndex
	}
	if next != manifest.MemorySize {
		return fmt.Errorf("build map does not cover memory")
	}
	return nil
}

func decodeStrictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}
