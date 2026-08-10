package memsnap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func writeJSONAtomic(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("create temporary JSON: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary JSON: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary JSON: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary JSON: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("rename temporary JSON: %w", err)
	}
	removeTemporary = false
	directoryFile, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open JSON directory: %w", err)
	}
	defer directoryFile.Close()
	if err := directoryFile.Sync(); err != nil {
		return fmt.Errorf("sync JSON directory: %w", err)
	}
	return nil
}

// WriteManifestAtomic durably replaces a manifest without exposing partial
// JSON to concurrent readers or crash recovery.
func WriteManifestAtomic(path string, manifest Manifest) error {
	if err := validateManifest(manifest); err != nil {
		return err
	}
	return writeJSONAtomic(path, manifest)
}
