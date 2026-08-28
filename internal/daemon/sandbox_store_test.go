package daemon

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/openeuler/Conch/internal/sandbox"
)

type memorySandboxStore struct {
	mu      sync.Mutex
	records map[string]sandbox.Record
}

func newMemorySandboxStore() *memorySandboxStore {
	return &memorySandboxStore{records: make(map[string]sandbox.Record)}
}

func (s *memorySandboxStore) Create(_ context.Context, record sandbox.Record) (sandbox.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[record.ID]; ok {
		return sandbox.Record{}, sandbox.ErrAlreadyExists.New()
	}
	if record.CreatedAt == 0 {
		record.CreatedAt = time.Now().UnixNano()
	}
	s.records[record.ID] = record
	return record, nil
}

func (s *memorySandboxStore) Update(_ context.Context, record sandbox.Record) (sandbox.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[record.ID]; !ok {
		return sandbox.Record{}, sandbox.ErrNotFound.New()
	}
	s.records[record.ID] = record
	return record, nil
}

func (s *memorySandboxStore) Get(_ context.Context, id string) (sandbox.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[id]
	if !ok {
		return sandbox.Record{}, sandbox.ErrNotFound.New()
	}
	return record, nil
}

func (s *memorySandboxStore) List(_ context.Context, filter sandbox.Filter) ([]sandbox.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make([]sandbox.Record, 0, len(s.records))
	for _, record := range s.records {
		if filter.State == "" || record.State == filter.State {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return records, nil
}

func (s *memorySandboxStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, id)
	return nil
}
