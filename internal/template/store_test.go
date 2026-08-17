package template

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
)

func TestStoreCRUDAndList(t *testing.T) {
	ctx := context.Background()
	raw := newMemoryStateStore()
	store := NewStore(raw)
	store.now = func() time.Time { return time.Unix(10, 0) }

	bootIndexDigest := digest.FromString("cold boot index").String()
	entry, err := store.Create(ctx, Entry{
		Origin:          OriginImage,
		BootMode:        BootModeCold,
		BootIndexDigest: bootIndexDigest,
		SourceRef:       "image-ref",
		Labels:          map[string]string{"purpose": "test"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if entry.CreatedAt != time.Unix(10, 0).UnixNano() {
		t.Fatalf("CreatedAt = %d", entry.CreatedAt)
	}

	got, err := store.Get(ctx, "  "+entry.BootIndexDigest+"  ")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.BootIndexDigest != bootIndexDigest || got.BootMode != BootModeCold {
		t.Fatalf("entry = %#v", got)
	}

	items, err := store.List(ctx, Filter{
		Origin:   OriginImage,
		BootMode: BootModeCold,
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(items) != 1 || items[0].BootIndexDigest != entry.BootIndexDigest {
		t.Fatalf("List() = %#v", items)
	}

	if err := store.Delete(ctx, "  "+entry.BootIndexDigest+"  "); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := store.Get(ctx, entry.BootIndexDigest); err == nil {
		t.Fatal("Get() after Delete() error = nil")
	}
}

func TestStoreCreateValidatesCompleteEntry(t *testing.T) {
	validDigest := digest.FromString("template").String()
	for _, tt := range []struct {
		name  string
		entry Entry
		want  string
	}{
		{
			name: "missing digest",
			entry: Entry{
				Origin: OriginImage, BootMode: BootModeCold,
			},
			want: "invalid boot index digest",
		},
		{
			name: "invalid origin",
			entry: Entry{
				Origin: "archive", BootMode: BootModeCold, BootIndexDigest: validDigest,
			},
			want: "unknown template origin",
		},
		{
			name: "invalid boot mode",
			entry: Entry{
				Origin: OriginImage, BootMode: "warm", BootIndexDigest: validDigest,
			},
			want: "unknown template boot mode",
		},
		{
			name: "invalid digest",
			entry: Entry{
				Origin: OriginImage, BootMode: BootModeCold, BootIndexDigest: "sha256:invalid",
			},
			want: "invalid boot index digest",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := NewStore(newMemoryStateStore())
			_, err := store.Create(context.Background(), tt.entry)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Create() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestStoreDuplicateDigestDoesNotOverwrite(t *testing.T) {
	ctx := context.Background()
	raw := newMemoryStateStore()
	store := NewStore(raw)
	firstDigest := digest.FromString("first").String()
	first, err := store.Create(ctx, Entry{
		Origin:          OriginImage,
		BootMode:        BootModeCold,
		BootIndexDigest: firstDigest,
	})
	if err != nil {
		t.Fatalf("first Create() error = %v", err)
	}

	_, err = store.Create(ctx, Entry{
		Origin:          OriginCheckpoint,
		BootMode:        BootModeResume,
		BootIndexDigest: first.BootIndexDigest,
	})
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("second Create() error = %v, want ErrAlreadyExists", err)
	}
	got, err := store.Get(ctx, first.BootIndexDigest)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.BootIndexDigest != firstDigest || got.Origin != OriginImage || got.BootMode != BootModeCold {
		t.Fatalf("first entry was overwritten: %#v", got)
	}
}

type memoryStateStore struct {
	entries map[string]Entry
}

func newMemoryStateStore() *memoryStateStore {
	return &memoryStateStore{entries: make(map[string]Entry)}
}

func (s *memoryStateStore) CreateTemplate(_ context.Context, entry Entry) error {
	if _, exists := s.entries[entry.BootIndexDigest]; exists {
		return ErrAlreadyExists
	}
	s.entries[entry.BootIndexDigest] = entry
	return nil
}

func (s *memoryStateStore) GetTemplate(_ context.Context, bootIndexDigest string) (Entry, error) {
	entry, exists := s.entries[bootIndexDigest]
	if !exists {
		return Entry{}, errors.New("not found")
	}
	return entry, nil
}

func (s *memoryStateStore) ListTemplates(context.Context) ([]Entry, error) {
	out := make([]Entry, 0, len(s.entries))
	for _, entry := range s.entries {
		out = append(out, entry)
	}
	return out, nil
}

func (s *memoryStateStore) DeleteTemplate(_ context.Context, bootIndexDigest string) error {
	delete(s.entries, bootIndexDigest)
	return nil
}
