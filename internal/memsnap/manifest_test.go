package memsnap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLoadAndPinRejectsDuplicateObjectKey(t *testing.T) {
	root := t.TempDir()
	raw := `{"schema_version":1,"memory_size":4096,"memory_size":8192,"block_size":4096,"layers":["layers/0.mem"],"build_map":[{"offset":0,"length":4096,"layer_index":0}]}`
	writeTestFile(t, filepath.Join(root, ManifestFileName), []byte(raw))

	pinned, err := LoadAndPin(root)
	if pinned != nil {
		_ = pinned.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "duplicate object key") {
		t.Fatalf("LoadAndPin() error = %v, want duplicate object key", err)
	}
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func validTestManifest() Manifest {
	return Manifest{
		SchemaVersion: SchemaVersion,
		MemorySize:    2 * DefaultBlockSize,
		BlockSize:     DefaultBlockSize,
		Layers:        []string{"layers/0.mem", "layers/1.mem"},
		BuildMap: []BuildRange{
			{Offset: 0, Length: DefaultBlockSize, LayerIndex: 0},
			{Offset: DefaultBlockSize, Length: DefaultBlockSize, LayerIndex: 1},
		},
	}
}

func writeTestManifest(t *testing.T, root string, manifest Manifest) {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, ManifestFileName), data)
}

func writeTestLayer(t *testing.T, root, name string, data []byte) {
	t.Helper()
	writeTestFile(t, filepath.Join(root, name), data)
}

func TestLoadAndPinResolvesSymlinkedRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "manifest")
	manifest := validTestManifest()
	manifest.Layers = []string{"layers/0.mem"}
	manifest.BuildMap = []BuildRange{{Offset: 0, Length: 2 * DefaultBlockSize, LayerIndex: 0}}
	layer := make([]byte, 2*DefaultBlockSize)
	layer[0] = 0x7a
	writeTestLayer(t, root, "layers/0.mem", layer)
	writeTestManifest(t, root, manifest)

	linkedRoot := filepath.Join(parent, "linked-manifest")
	if err := os.Symlink(root, linkedRoot); err != nil {
		t.Fatal(err)
	}
	pinned, err := LoadAndPin(linkedRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pinned.Close() })
	page := make([]byte, DefaultBlockSize)
	if err := pinned.ReadPage(0, page); err != nil {
		t.Fatal(err)
	}
	if page[0] != 0x7a {
		t.Fatalf("ReadPage() first byte = %#x, want %#x", page[0], byte(0x7a))
	}
}

func TestLoadAndPinRejectsSymlinkedLayerUnderResolvedRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "manifest")
	manifest := validTestManifest()
	manifest.Layers = []string{"layers/0.mem"}
	manifest.BuildMap = []BuildRange{{Offset: 0, Length: 2 * DefaultBlockSize, LayerIndex: 0}}
	writeTestLayer(t, root, "layers/target.mem", make([]byte, 2*DefaultBlockSize))
	if err := os.Symlink("target.mem", filepath.Join(root, "layers/0.mem")); err != nil {
		t.Fatal(err)
	}
	writeTestManifest(t, root, manifest)

	linkedRoot := filepath.Join(parent, "linked-manifest")
	if err := os.Symlink(root, linkedRoot); err != nil {
		t.Fatal(err)
	}
	pinned, err := LoadAndPin(linkedRoot)
	if pinned != nil {
		_ = pinned.Close()
	}
	if err == nil || !strings.Contains(err.Error(), `path leaf "0.mem" is not a regular file`) {
		t.Fatalf("LoadAndPin() error = %v, want symlinked layer rejection", err)
	}
}

func TestLoadAndPinRejectsUnknownAndTrailingJSON(t *testing.T) {
	manifest := validTestManifest()
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		raw  string
	}{
		{name: "unknown field", raw: strings.Replace(string(raw), "{", `{"unexpected":true,`, 1)},
		{name: "trailing value", raw: string(raw) + `{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeTestFile(t, filepath.Join(root, ManifestFileName), []byte(test.raw))
			if pinned, err := LoadAndPin(root); err == nil {
				_ = pinned.Close()
				t.Fatal("LoadAndPin accepted ambiguous JSON")
			}
		})
	}
}

func TestLoadAndPinRejectsInvalidGeometryAndBuildMap(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{name: "schema", mutate: func(m *Manifest) { m.SchemaVersion = 2 }},
		{name: "zero memory", mutate: func(m *Manifest) { m.MemorySize = 0 }},
		{name: "zero block", mutate: func(m *Manifest) { m.BlockSize = 0 }},
		{name: "misaligned memory", mutate: func(m *Manifest) { m.MemorySize++ }},
		{name: "gap", mutate: func(m *Manifest) { m.BuildMap = m.BuildMap[:1] }},
		{name: "overlap", mutate: func(m *Manifest) { m.BuildMap[1].Offset = 0 }},
		{name: "undeclared layer", mutate: func(m *Manifest) { m.BuildMap[0].LayerIndex = 2 }},
		{name: "unmerged", mutate: func(m *Manifest) { m.BuildMap[1].LayerIndex = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			manifest := validTestManifest()
			test.mutate(&manifest)
			writeTestManifest(t, root, manifest)
			writeTestLayer(t, root, "layers/0.mem", make([]byte, 2*DefaultBlockSize))
			writeTestLayer(t, root, "layers/1.mem", make([]byte, 2*DefaultBlockSize))
			if pinned, err := LoadAndPin(root); err == nil {
				_ = pinned.Close()
				t.Fatal("LoadAndPin accepted an invalid manifest")
			}
		})
	}
}

func TestLoadAndPinRejectsUnsafeLayers(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		setup func(*testing.T, string)
	}{
		{name: "absolute", path: "/tmp/0.mem"},
		{name: "traversal", path: "../0.mem"},
		{name: "unclean", path: "layers/../layers/0.mem"},
		{name: "wrong name", path: "layers/first.mem"},
		{name: "symlink", path: "layers/0.mem", setup: func(t *testing.T, root string) {
			writeTestLayer(t, root, "layers/target.mem", make([]byte, 2*DefaultBlockSize))
			if err := os.Symlink("target.mem", filepath.Join(root, "layers/0.mem")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wrong size", path: "layers/0.mem", setup: func(t *testing.T, root string) {
			writeTestLayer(t, root, "layers/0.mem", make([]byte, DefaultBlockSize))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			manifest := validTestManifest()
			manifest.Layers = []string{test.path}
			manifest.BuildMap = []BuildRange{{Offset: 0, Length: 2 * DefaultBlockSize, LayerIndex: 0}}
			if test.setup != nil {
				test.setup(t, root)
			}
			writeTestManifest(t, root, manifest)
			if pinned, err := LoadAndPin(root); err == nil {
				_ = pinned.Close()
				t.Fatal("LoadAndPin accepted an unsafe layer")
			}
		})
	}
}

func TestLoadAndPinKeepsValidatedLayerOpen(t *testing.T) {
	root := t.TempDir()
	manifest := validTestManifest()
	manifest.Layers = []string{"layers/0.mem"}
	manifest.BuildMap = []BuildRange{{Offset: 0, Length: 2 * DefaultBlockSize, LayerIndex: 0}}
	original := make([]byte, 2*DefaultBlockSize)
	original[0] = 0x7a
	writeTestLayer(t, root, "layers/0.mem", original)
	writeTestManifest(t, root, manifest)

	pinned, err := LoadAndPin(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pinned.Close() })
	if pinned.manifestFile == nil {
		t.Fatal("LoadAndPin() did not keep the validated manifest open")
	}
	path := filepath.Join(root, "layers/0.mem")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	replacement := make([]byte, 2*DefaultBlockSize)
	replacement[0] = 0x19
	writeTestFile(t, path, replacement)
	page := make([]byte, DefaultBlockSize)
	if err := pinned.ReadPage(0, page); err != nil {
		t.Fatal(err)
	}
	if page[0] != 0x7a {
		t.Fatalf("ReadPage() first byte = %#x, want %#x", page[0], byte(0x7a))
	}
}

func TestLoadAndPinRejectsLayerReplacedDuringOpen(t *testing.T) {
	root := t.TempDir()
	manifest := validTestManifest()
	manifest.Layers = []string{"layers/0.mem"}
	manifest.BuildMap = []BuildRange{{Offset: 0, Length: 2 * DefaultBlockSize, LayerIndex: 0}}
	writeTestLayer(t, root, "layers/0.mem", make([]byte, 2*DefaultBlockSize))
	writeTestManifest(t, root, manifest)

	realOpenat := openatForPin
	openatForPin = func(dirfd int, path string, flags int, mode uint32) (int, error) {
		if path == "0.mem" {
			leaf := filepath.Join(root, "layers/0.mem")
			if err := os.Rename(leaf, leaf+".original"); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, leaf, make([]byte, 2*DefaultBlockSize))
		}
		return unix.Openat(dirfd, path, flags, mode)
	}
	t.Cleanup(func() { openatForPin = realOpenat })

	if pinned, err := LoadAndPin(root); err == nil {
		_ = pinned.Close()
		t.Fatal("LoadAndPin accepted a replaced layer inode")
	}
}

func TestLoadAndPinEpochPinsOnlyNewestLayer(t *testing.T) {
	root := t.TempDir()
	manifest := validTestManifest()
	writeTestLayer(t, root, "layers/1.mem", make([]byte, 2*DefaultBlockSize))
	writeTestManifest(t, root, manifest)

	pinned, err := LoadAndPinEpoch(root)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	page := make([]byte, DefaultBlockSize)
	if err := pinned.ReadPage(DefaultBlockSize, page); err != nil {
		t.Fatalf("ReadPage(newest layer): %v", err)
	}
	if err := pinned.ReadPage(0, page); err == nil {
		t.Fatal("ReadPage unexpectedly read an unpinned parent layer")
	}
}
