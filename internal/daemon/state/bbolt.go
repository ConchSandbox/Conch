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

	conchtemplate "github.com/openeuler/Conch/internal/template"
)

var ErrNotFound = errors.New("state record not found")

var buckets = [][]byte{
	[]byte("sandboxes"),
	[]byte("templates"),
}

type BoltStore struct {
	db *bolt.DB
}

type templateRecord struct {
	Origin                string            `json:"origin"`
	BootMode              string            `json:"boot_mode"`
	BootIndexDigest       string            `json:"boot_index_digest"`
	ParentBootIndexDigest string            `json:"parent_boot_index_digest,omitempty"`
	SourceSandboxID       string            `json:"source_sandbox_id,omitempty"`
	SourceRef             string            `json:"source_ref,omitempty"`
	Labels                map[string]string `json:"labels,omitempty"`
	CreatedAt             int64             `json:"created_at"`
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
	if strings.TrimSpace(key) == "" {
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
	if strings.TrimSpace(key) == "" {
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
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("state key is required")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Delete([]byte(key))
	})
}

func (s *BoltStore) UpsertSandbox(ctx context.Context, rec SandboxRecord) error {
	if strings.TrimSpace(rec.SandboxID) == "" {
		return fmt.Errorf("sandbox id is required")
	}
	if rec.State == SandboxCreating {
		if strings.TrimSpace(rec.SourceTemplateID) == "" {
			return fmt.Errorf("creating sandbox source Template ID is required")
		}
		return s.upsert(ctx, []byte("sandboxes"), rec.SandboxID, rec)
	}
	if strings.TrimSpace(rec.CheckpointHeadTemplateID) == "" {
		return fmt.Errorf("sandbox checkpoint head Template ID is required")
	}
	return s.upsert(ctx, []byte("sandboxes"), rec.SandboxID, rec)
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

func (s *BoltStore) CreateTemplate(_ context.Context, entry conchtemplate.Entry) error {
	rec := templateRecordFromEntry(entry)
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal template entry: %w", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		templates := tx.Bucket([]byte("templates"))
		if templates.Get([]byte(entry.BootIndexDigest)) != nil {
			return fmt.Errorf("%w: %s", conchtemplate.ErrAlreadyExists, entry.BootIndexDigest)
		}
		return templates.Put([]byte(entry.BootIndexDigest), data)
	})
}

func (s *BoltStore) GetTemplate(ctx context.Context, bootIndexDigest string) (conchtemplate.Entry, error) {
	var rec templateRecord
	if err := s.get(ctx, []byte("templates"), bootIndexDigest, &rec); err != nil {
		if errors.Is(err, ErrNotFound) {
			return conchtemplate.Entry{}, conchtemplate.ErrNotFound.Wrap(err)
		}
		return conchtemplate.Entry{}, err
	}
	return templateEntryFromRecord(rec), nil
}

func (s *BoltStore) ListTemplates(ctx context.Context) ([]conchtemplate.Entry, error) {
	var out []conchtemplate.Entry
	err := s.list(ctx, []byte("templates"), func(data []byte) error {
		var rec templateRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		out = append(out, templateEntryFromRecord(rec))
		return nil
	})
	return out, err
}

func (s *BoltStore) DeleteTemplate(ctx context.Context, bootIndexDigest string) error {
	return s.delete(ctx, []byte("templates"), bootIndexDigest)
}

func (s *BoltStore) AdvanceCheckpointHead(_ context.Context, sandboxID, expectedDigest, nextDigest string) error {
	sandboxID = strings.TrimSpace(sandboxID)
	expectedDigest = strings.TrimSpace(expectedDigest)
	nextDigest = strings.TrimSpace(nextDigest)
	if sandboxID == "" || expectedDigest == "" || nextDigest == "" {
		return fmt.Errorf("sandbox id and checkpoint head digests are required")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		sandboxes := tx.Bucket([]byte("sandboxes"))
		data := sandboxes.Get([]byte(sandboxID))
		if data == nil {
			return fmt.Errorf("%w: %s", ErrNotFound, sandboxID)
		}
		var record SandboxRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return fmt.Errorf("unmarshal sandbox record %s: %w", sandboxID, err)
		}
		if record.CheckpointHeadTemplateID != expectedDigest {
			return fmt.Errorf("sandbox %s checkpoint head changed from %s to %s", sandboxID, expectedDigest, record.CheckpointHeadTemplateID)
		}
		record.CheckpointHeadTemplateID = nextDigest
		data, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("marshal sandbox record: %w", err)
		}
		return sandboxes.Put([]byte(sandboxID), data)
	})
}

// PublishCheckpoint atomically creates a complete checkpoint Template Entry
// and advances the Sandbox checkpoint head. Content publication and validation
// happen before this transaction, so a failed transaction can only leave safe
// orphaned content.
func (s *BoltStore) PublishCheckpoint(_ context.Context, checkpoint conchtemplate.Entry) error {
	entry := checkpoint
	bootIndexDigest := entry.BootIndexDigest
	sandboxID := strings.TrimSpace(entry.SourceSandboxID)
	if sandboxID == "" {
		return fmt.Errorf("sandbox id is required")
	}
	expectedHeadDigest := strings.TrimSpace(entry.ParentBootIndexDigest)
	if expectedHeadDigest == "" {
		return fmt.Errorf("expected checkpoint head boot index digest is required")
	}
	if entry.Origin != conchtemplate.OriginCheckpoint {
		return fmt.Errorf(
			"checkpoint template origin is %q, want %q",
			entry.Origin,
			conchtemplate.OriginCheckpoint,
		)
	}
	if entry.BootMode != conchtemplate.BootModeResume {
		return fmt.Errorf(
			"checkpoint template boot mode is %q, want %q",
			entry.BootMode,
			conchtemplate.BootModeResume,
		)
	}
	templateData, err := json.Marshal(templateRecordFromEntry(entry))
	if err != nil {
		return fmt.Errorf("marshal template entry: %w", err)
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		templates := tx.Bucket([]byte("templates"))
		sandboxes := tx.Bucket([]byte("sandboxes"))
		if templates.Get([]byte(bootIndexDigest)) != nil {
			return fmt.Errorf("%w: %s", conchtemplate.ErrAlreadyExists, bootIndexDigest)
		}

		var sandboxRecord SandboxRecord
		if data := sandboxes.Get([]byte(sandboxID)); data == nil {
			return fmt.Errorf("%w: %s", ErrNotFound, sandboxID)
		} else if err := json.Unmarshal(data, &sandboxRecord); err != nil {
			return fmt.Errorf("unmarshal sandbox record %s: %w", sandboxID, err)
		}
		currentHeadDigest := strings.TrimSpace(sandboxRecord.CheckpointHeadTemplateID)
		if currentHeadDigest == "" {
			return fmt.Errorf("sandbox %s has no checkpoint head Template ID", sandboxID)
		}
		if currentHeadDigest != expectedHeadDigest {
			return fmt.Errorf("sandbox %s checkpoint head boot index digest changed from %s to %s", sandboxID, expectedHeadDigest, currentHeadDigest)
		}
		if sourceID := entry.SourceSandboxID; sourceID != strings.TrimSpace(sandboxRecord.SandboxID) {
			return fmt.Errorf("template %s source sandbox %s does not match %s", bootIndexDigest, sourceID, sandboxRecord.SandboxID)
		}
		sandboxRecord.CheckpointHeadTemplateID = entry.BootIndexDigest
		sandboxData, err := json.Marshal(sandboxRecord)
		if err != nil {
			return fmt.Errorf("marshal sandbox record: %w", err)
		}
		if err := templates.Put([]byte(bootIndexDigest), templateData); err != nil {
			return err
		}
		return sandboxes.Put([]byte(sandboxID), sandboxData)
	})
}

func templateRecordFromEntry(entry conchtemplate.Entry) templateRecord {
	return templateRecord{
		Origin:                string(entry.Origin),
		BootMode:              string(entry.BootMode),
		BootIndexDigest:       entry.BootIndexDigest,
		ParentBootIndexDigest: entry.ParentBootIndexDigest,
		SourceSandboxID:       entry.SourceSandboxID,
		SourceRef:             entry.SourceRef,
		Labels:                entry.Labels,
		CreatedAt:             entry.CreatedAt,
	}
}

func templateEntryFromRecord(rec templateRecord) conchtemplate.Entry {
	return conchtemplate.Entry{
		Origin:                conchtemplate.Origin(rec.Origin),
		BootMode:              conchtemplate.BootMode(rec.BootMode),
		BootIndexDigest:       rec.BootIndexDigest,
		ParentBootIndexDigest: rec.ParentBootIndexDigest,
		SourceSandboxID:       rec.SourceSandboxID,
		SourceRef:             rec.SourceRef,
		Labels:                rec.Labels,
		CreatedAt:             rec.CreatedAt,
	}
}
