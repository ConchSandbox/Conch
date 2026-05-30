package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

var ErrNotFound = errors.New("state record not found")

var buckets = [][]byte{
	[]byte("sandboxes"),
	[]byte("containers"),
	[]byte("snapshot_runtimes"),
	[]byte("view_snapshots"),
	[]byte("view_aliases"),
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

func (s *BoltStore) UpsertSnapshotRuntime(ctx context.Context, rec SnapshotRuntimeRecord) error {
	return s.upsert(ctx, []byte("snapshot_runtimes"), namespaceKey(rec.Namespace, rec.SandboxID), rec)
}

func (s *BoltStore) ListSnapshotRuntimes(ctx context.Context) ([]SnapshotRuntimeRecord, error) {
	var out []SnapshotRuntimeRecord
	err := s.list(ctx, []byte("snapshot_runtimes"), func(data []byte) error {
		var rec SnapshotRuntimeRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		out = append(out, rec)
		return nil
	})
	return out, err
}

func (s *BoltStore) DeleteSnapshotRuntime(ctx context.Context, namespace, sandboxID string) error {
	return s.delete(ctx, []byte("snapshot_runtimes"), namespaceKey(namespace, sandboxID))
}

func (s *BoltStore) UpsertViewSnapshot(ctx context.Context, rec ViewSnapshotRecord) error {
	return s.upsert(ctx, []byte("view_snapshots"), namespaceKey(rec.Namespace, rec.ParentSnapshotID), rec)
}

func (s *BoltStore) ListViewSnapshots(ctx context.Context) ([]ViewSnapshotRecord, error) {
	var out []ViewSnapshotRecord
	err := s.list(ctx, []byte("view_snapshots"), func(data []byte) error {
		var rec ViewSnapshotRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		out = append(out, rec)
		return nil
	})
	return out, err
}

func (s *BoltStore) DeleteViewSnapshot(ctx context.Context, namespace, parentSnapshotID string) error {
	return s.delete(ctx, []byte("view_snapshots"), namespaceKey(namespace, parentSnapshotID))
}

func (s *BoltStore) UpsertViewAlias(ctx context.Context, rec ViewAliasRecord) error {
	return s.upsert(ctx, []byte("view_aliases"), namespaceKey(rec.Namespace, rec.AliasKey), rec)
}

func (s *BoltStore) ListViewAliases(ctx context.Context) ([]ViewAliasRecord, error) {
	var out []ViewAliasRecord
	err := s.list(ctx, []byte("view_aliases"), func(data []byte) error {
		var rec ViewAliasRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		out = append(out, rec)
		return nil
	})
	return out, err
}

func (s *BoltStore) DeleteViewAlias(ctx context.Context, namespace, aliasKey string) error {
	return s.delete(ctx, []byte("view_aliases"), namespaceKey(namespace, aliasKey))
}

func namespaceKey(namespace, key string) string {
	return strings.TrimSpace(namespace) + "/" + strings.TrimSpace(key)
}
