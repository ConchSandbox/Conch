package template

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type Filter struct {
	Origin   Origin
	BootMode BootMode
}

type Store interface {
	Create(context.Context, Entry) (Entry, error)
	Get(context.Context, string) (Entry, error)
	List(context.Context, Filter) ([]Entry, error)
}

// StateStore is the persistence capability required by the Template domain.
// Implementations must provide insert-only CreateTemplate semantics.
type StateStore interface {
	CreateTemplate(context.Context, Entry) error
	GetTemplate(context.Context, string) (Entry, error)
	ListTemplates(context.Context) ([]Entry, error)
}

type PersistentStore struct {
	store StateStore
	now   func() time.Time
}

func NewStore(store StateStore) *PersistentStore {
	return &PersistentStore{
		store: store,
		now:   time.Now,
	}
}

func (s *PersistentStore) Create(ctx context.Context, entry Entry) (Entry, error) {
	if s == nil || s.store == nil {
		return Entry{}, fmt.Errorf("template store is not configured")
	}
	if entry.CreatedAt == 0 {
		entry.CreatedAt = s.now().UnixNano()
	}
	normalized, err := NormalizeEntry(entry)
	if err != nil {
		return Entry{}, err
	}
	if err := s.store.CreateTemplate(ctx, normalized); err != nil {
		return Entry{}, err
	}
	return normalized, nil
}

func (s *PersistentStore) Get(ctx context.Context, id string) (Entry, error) {
	if s == nil || s.store == nil {
		return Entry{}, fmt.Errorf("template store is not configured")
	}
	entry, err := s.store.GetTemplate(ctx, strings.TrimSpace(id))
	if err != nil {
		return Entry{}, err
	}
	return NormalizeEntry(entry)
}

func (s *PersistentStore) List(ctx context.Context, filter Filter) ([]Entry, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("template store is not configured")
	}
	if filter.Origin != "" {
		switch filter.Origin {
		case OriginImage, OriginCheckpoint:
		default:
			return nil, fmt.Errorf("unknown template origin %q", filter.Origin)
		}
	}
	if filter.BootMode != "" {
		switch filter.BootMode {
		case BootModeCold, BootModeResume:
		default:
			return nil, fmt.Errorf("unknown template boot mode %q", filter.BootMode)
		}
	}
	items, err := s.store.ListTemplates(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(items))
	for _, raw := range items {
		item, err := NormalizeEntry(raw)
		if err != nil {
			return nil, err
		}
		if filter.Origin != "" && item.Origin != filter.Origin {
			continue
		}
		if filter.BootMode != "" && item.BootMode != filter.BootMode {
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

func copyMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
