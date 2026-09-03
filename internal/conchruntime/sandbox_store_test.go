package conchruntime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/openeuler/Conch/internal/sandbox"
)

type memorySandboxStore struct {
	mu         sync.Mutex
	records    map[string]sandbox.Record
	operations []string
}

func newMemorySandboxStore() *memorySandboxStore {
	return &memorySandboxStore{records: make(map[string]sandbox.Record)}
}

func (s *memorySandboxStore) Create(_ context.Context, record sandbox.Record) (sandbox.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateMemorySandboxRecord(record); err != nil {
		return sandbox.Record{}, err
	}
	if _, ok := s.records[record.ID]; ok {
		return sandbox.Record{}, sandbox.ErrAlreadyExists.New()
	}
	if record.CreatedAt == 0 {
		record.CreatedAt = time.Now().UnixNano()
	}
	s.records[record.ID] = record
	s.operations = append(s.operations, "create:"+string(record.State))
	return record, nil
}

func (s *memorySandboxStore) Update(_ context.Context, record sandbox.Record) (sandbox.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateMemorySandboxRecord(record); err != nil {
		return sandbox.Record{}, err
	}
	if _, ok := s.records[record.ID]; !ok {
		return sandbox.Record{}, sandbox.ErrNotFound.New()
	}
	s.records[record.ID] = record
	s.operations = append(s.operations, "update:"+string(record.State))
	return record, nil
}

func validateMemorySandboxRecord(record sandbox.Record) error {
	if record.State == sandbox.StateCreating {
		if record.SourceTemplateID == "" {
			return sandbox.ErrInvalidArgument.New()
		}
		return nil
	}
	if record.CheckpointHeadTemplateID == "" {
		return sandbox.ErrInvalidArgument.New()
	}
	return nil
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
	return records, nil
}

func (s *memorySandboxStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, id)
	s.operations = append(s.operations, "delete")
	return nil
}

func (s *memorySandboxStore) operationLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.operations...)
}

func (s *memorySandboxStore) Put(ctx context.Context, record sandbox.Record) error {
	if _, err := s.Get(ctx, record.ID); errors.Is(err, sandbox.ErrNotFound) {
		_, err = s.Create(ctx, record)
		return err
	}
	_, err := s.Update(ctx, record)
	return err
}
