package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

var ErrNotFound = errors.New("state record not found")

var buckets = [][]byte{
	[]byte("sandboxes"),
	[]byte("network_slots"),
	[]byte("containers"),
	[]byte("templates"),
}

type BoltStore struct {
	db *bolt.DB
}

func OpenBolt(path string) (*BoltStore, error) {
	if path == "" {
		return nil, fmt.Errorf("state db path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create state db dir: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}
	store := &BoltStore{db: db}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *BoltStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *BoltStore) init() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, bucket := range buckets {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return fmt.Errorf("create bucket %s: %w", bucket, err)
			}
		}
		return nil
	})
}

func (s *BoltStore) upsert(_ context.Context, bucket []byte, key string, value any) error {
	if key == "" {
		return fmt.Errorf("state key is required")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal state record: %w", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Put([]byte(key), data)
	})
}

func (s *BoltStore) get(_ context.Context, bucket []byte, key string, value any) error {
	if key == "" {
		return fmt.Errorf("state key is required")
	}
	return s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(bucket).Get([]byte(key))
		if data == nil {
			return fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		if err := json.Unmarshal(data, value); err != nil {
			return fmt.Errorf("unmarshal state record %s: %w", key, err)
		}
		return nil
	})
}

func (s *BoltStore) list(_ context.Context, bucket []byte, appendValue func([]byte) error) error {
	return s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).ForEach(func(_, data []byte) error {
			return appendValue(data)
		})
	})
}

func (s *BoltStore) delete(_ context.Context, bucket []byte, key string) error {
	if key == "" {
		return fmt.Errorf("state key is required")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Delete([]byte(key))
	})
}

func (s *BoltStore) UpsertSandbox(ctx context.Context, rec SandboxRecord) error {
	return s.upsert(ctx, []byte("sandboxes"), rec.PodSandboxID, rec)
}

func (s *BoltStore) GetSandbox(ctx context.Context, id string) (SandboxRecord, error) {
	var rec SandboxRecord
	err := s.get(ctx, []byte("sandboxes"), id, &rec)
	return rec, err
}

func (s *BoltStore) ListSandboxes(ctx context.Context) ([]SandboxRecord, error) {
	var out []SandboxRecord
	err := s.list(ctx, []byte("sandboxes"), func(data []byte) error {
		var rec SandboxRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		out = append(out, rec)
		return nil
	})
	return out, err
}

func (s *BoltStore) DeleteSandbox(ctx context.Context, id string) error {
	return s.delete(ctx, []byte("sandboxes"), id)
}

func (s *BoltStore) UpsertNetworkSlot(ctx context.Context, rec NetworkSlotRecord) error {
	return s.upsert(ctx, []byte("network_slots"), rec.SlotKey, rec)
}

func (s *BoltStore) GetNetworkSlot(ctx context.Context, slotKey string) (NetworkSlotRecord, error) {
	var rec NetworkSlotRecord
	err := s.get(ctx, []byte("network_slots"), slotKey, &rec)
	return rec, err
}

func (s *BoltStore) ListNetworkSlots(ctx context.Context) ([]NetworkSlotRecord, error) {
	var out []NetworkSlotRecord
	err := s.list(ctx, []byte("network_slots"), func(data []byte) error {
		var rec NetworkSlotRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		out = append(out, rec)
		return nil
	})
	return out, err
}

func (s *BoltStore) DeleteNetworkSlot(ctx context.Context, slotKey string) error {
	return s.delete(ctx, []byte("network_slots"), slotKey)
}

func (s *BoltStore) UpsertContainer(ctx context.Context, rec ContainerRecord) error {
	return s.upsert(ctx, []byte("containers"), rec.ContainerID, rec)
}

func (s *BoltStore) GetContainer(ctx context.Context, id string) (ContainerRecord, error) {
	var rec ContainerRecord
	err := s.get(ctx, []byte("containers"), id, &rec)
	return rec, err
}

func (s *BoltStore) ListContainers(ctx context.Context) ([]ContainerRecord, error) {
	var out []ContainerRecord
	err := s.list(ctx, []byte("containers"), func(data []byte) error {
		var rec ContainerRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		out = append(out, rec)
		return nil
	})
	return out, err
}

func (s *BoltStore) DeleteContainer(ctx context.Context, id string) error {
	return s.delete(ctx, []byte("containers"), id)
}

func (s *BoltStore) UpsertTemplate(ctx context.Context, rec TemplateRecord) error {
	return s.upsert(ctx, []byte("templates"), rec.ID, rec)
}

func (s *BoltStore) GetTemplate(ctx context.Context, id string) (TemplateRecord, error) {
	var rec TemplateRecord
	err := s.get(ctx, []byte("templates"), id, &rec)
	return rec, err
}

func (s *BoltStore) ListTemplates(ctx context.Context) ([]TemplateRecord, error) {
	var out []TemplateRecord
	err := s.list(ctx, []byte("templates"), func(data []byte) error {
		var rec TemplateRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		out = append(out, rec)
		return nil
	})
	return out, err
}

func (s *BoltStore) DeleteTemplate(ctx context.Context, id string) error {
	return s.delete(ctx, []byte("templates"), id)
}
