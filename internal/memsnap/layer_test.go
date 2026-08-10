package memsnap

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func testPage(value byte) []byte {
	return bytes.Repeat([]byte{value}, int(DefaultBlockSize))
}

func TestCreateBaseLayerOwnsEveryPageAtGuestOffsets(t *testing.T) {
	root := t.TempDir()
	manifest, err := CreateBaseLayer(root, 4*DefaultBlockSize, DefaultBlockSize, func(sink PageSink) error {
		if err := sink.WritePage(0, testPage(0x10)); err != nil {
			return err
		}
		if err := sink.WriteZeroPage(DefaultBlockSize); err != nil {
			return err
		}
		if err := sink.WritePage(2*DefaultBlockSize, testPage(0x30)); err != nil {
			return err
		}
		return sink.WriteZeroPage(3 * DefaultBlockSize)
	})
	if err != nil {
		t.Fatal(err)
	}
	wantMap := []BuildRange{{Offset: 0, Length: 4 * DefaultBlockSize, LayerIndex: 0}}
	if !reflect.DeepEqual(manifest.BuildMap, wantMap) {
		t.Fatalf("BuildMap = %#v, want %#v", manifest.BuildMap, wantMap)
	}
	data, err := os.ReadFile(filepath.Join(root, "layers/0.mem"))
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(data)) != 4*DefaultBlockSize {
		t.Fatalf("layer size = %d, want %d", len(data), 4*DefaultBlockSize)
	}
	if data[0] != 0x10 || data[DefaultBlockSize] != 0 || data[2*DefaultBlockSize] != 0x30 {
		t.Fatalf("unexpected bytes at guest offsets: %#x %#x %#x", data[0], data[DefaultBlockSize], data[2*DefaultBlockSize])
	}
}

func TestCreateDeltaLayerReplacesOnlyDirtyOwnership(t *testing.T) {
	root := t.TempDir()
	base, err := CreateBaseLayer(root, 4*DefaultBlockSize, DefaultBlockSize, func(sink PageSink) error {
		for offset := uint64(0); offset < 4*DefaultBlockSize; offset += DefaultBlockSize {
			if err := sink.WritePage(offset, testPage(byte(0x10+offset/DefaultBlockSize))); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	delta, err := CreateDeltaLayer(base, root, func(sink PageSink) error {
		if err := sink.WritePage(DefaultBlockSize, testPage(0x77)); err != nil {
			return err
		}
		return sink.WriteZeroPage(2 * DefaultBlockSize)
	})
	if err != nil {
		t.Fatal(err)
	}
	wantMap := []BuildRange{
		{Offset: 0, Length: DefaultBlockSize, LayerIndex: 0},
		{Offset: DefaultBlockSize, Length: 2 * DefaultBlockSize, LayerIndex: 1},
		{Offset: 3 * DefaultBlockSize, Length: DefaultBlockSize, LayerIndex: 0},
	}
	if !reflect.DeepEqual(delta.BuildMap, wantMap) {
		t.Fatalf("BuildMap = %#v, want %#v", delta.BuildMap, wantMap)
	}
	data, err := os.ReadFile(filepath.Join(root, "layers/1.mem"))
	if err != nil {
		t.Fatal(err)
	}
	if data[DefaultBlockSize] != 0x77 || data[2*DefaultBlockSize] != 0 {
		t.Fatalf("delta bytes = %#x %#x", data[DefaultBlockSize], data[2*DefaultBlockSize])
	}
}

func TestCreateBaseLayerRejectsInvalidWrites(t *testing.T) {
	tests := []struct {
		name   string
		export func(PageSink) error
	}{
		{name: "duplicate", export: func(sink PageSink) error {
			if err := sink.WritePage(0, testPage(1)); err != nil {
				return err
			}
			return sink.WriteZeroPage(0)
		}},
		{name: "unaligned", export: func(sink PageSink) error { return sink.WritePage(1, testPage(1)) }},
		{name: "out of range", export: func(sink PageSink) error { return sink.WriteZeroPage(4 * DefaultBlockSize) }},
		{name: "short page", export: func(sink PageSink) error { return sink.WritePage(0, []byte{1}) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := CreateBaseLayer(t.TempDir(), 4*DefaultBlockSize, DefaultBlockSize, test.export); err == nil {
				t.Fatal("CreateBaseLayer accepted an invalid write")
			}
		})
	}
}

func TestWriteManifestAtomicPublishesLoadableManifest(t *testing.T) {
	root := t.TempDir()
	manifest := validTestManifest()
	writeTestLayer(t, root, "layers/0.mem", make([]byte, 2*DefaultBlockSize))
	writeTestLayer(t, root, "layers/1.mem", make([]byte, 2*DefaultBlockSize))
	if err := WriteManifestAtomic(filepath.Join(root, ManifestFileName), manifest); err != nil {
		t.Fatal(err)
	}
	pinned, err := LoadAndPin(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := pinned.Close(); err != nil {
		t.Fatal(err)
	}
}
