package memsnap

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// PinnedManifest keeps the validated manifest and layer descriptors open.
// Replacing a path after validation therefore cannot change the bytes returned
// to the guest.
type PinnedManifest struct {
	Manifest     Manifest
	manifestFile *os.File
	layers       map[int]*os.File
}

func LoadAndPin(root string) (*PinnedManifest, error) {
	manifestFile, err := os.Open(filepath.Join(root, ManifestFileName))
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	data, readErr := io.ReadAll(manifestFile)
	if readErr != nil {
		closeErr := manifestFile.Close()
		if closeErr != nil {
			return nil, errors.Join(fmt.Errorf("read manifest: %w", readErr), fmt.Errorf("close manifest: %w", closeErr))
		}
		return nil, fmt.Errorf("read manifest: %w", readErr)
	}
	var manifest Manifest
	if err := decodeStrictJSON(data, &manifest); err != nil {
		_ = manifestFile.Close()
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := validateManifest(manifest); err != nil {
		_ = manifestFile.Close()
		return nil, err
	}

	pinned := &PinnedManifest{Manifest: manifest, manifestFile: manifestFile, layers: make(map[int]*os.File, len(manifest.Layers))}
	for index, layer := range manifest.Layers {
		file, err := os.Open(filepath.Join(root, filepath.FromSlash(layer)))
		if err != nil {
			_ = pinned.Close()
			return nil, fmt.Errorf("open layer %d: %w", index, err)
		}
		stat, err := file.Stat()
		if err != nil {
			_ = file.Close()
			_ = pinned.Close()
			return nil, fmt.Errorf("stat layer %d: %w", index, err)
		}
		if stat.Size() < 0 || uint64(stat.Size()) != manifest.MemorySize {
			_ = file.Close()
			_ = pinned.Close()
			return nil, fmt.Errorf("layer %d has invalid logical size", index)
		}
		pinned.layers[index] = file
	}
	return pinned, nil
}

func (p *PinnedManifest) ReadPage(offset uint64, dst []byte) error {
	if p == nil {
		return errors.New("nil pinned manifest")
	}
	if uint64(len(dst)) != p.Manifest.BlockSize || offset%p.Manifest.BlockSize != 0 || offset >= p.Manifest.MemorySize {
		return fmt.Errorf("invalid page offset or size")
	}
	index := sort.Search(len(p.Manifest.BuildMap), func(index int) bool {
		span := p.Manifest.BuildMap[index]
		return span.Offset+span.Length > offset
	})
	if index == len(p.Manifest.BuildMap) || offset < p.Manifest.BuildMap[index].Offset {
		return fmt.Errorf("no layer owns page at offset %d", offset)
	}
	layerIndex := p.Manifest.BuildMap[index].LayerIndex
	file := p.layers[layerIndex]
	if file == nil {
		return fmt.Errorf("layer %d is not pinned", layerIndex)
	}
	if _, err := io.ReadFull(io.NewSectionReader(file, int64(offset), int64(len(dst))), dst); err != nil {
		return fmt.Errorf("read page at offset %d: %w", offset, err)
	}
	return nil
}

func (p *PinnedManifest) Close() error {
	if p == nil {
		return nil
	}
	var result error
	if p.manifestFile != nil {
		if err := p.manifestFile.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close manifest: %w", err))
		}
		p.manifestFile = nil
	}
	for index, file := range p.layers {
		if err := file.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("close layer %d: %w", index, err))
		}
		delete(p.layers, index)
	}
	return result
}
