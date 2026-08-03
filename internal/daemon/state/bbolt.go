package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	conchtemplate "github.com/openeuler/Conch/internal/template"
)

var (
	ErrNotFound      = errors.New("state record not found")
	ErrAlreadyExists = errors.New("state record already exists")
)

var buckets = [][]byte{
	[]byte("sandboxes"),
	[]byte("network_slots"),
	[]byte("templates"),
}

type BoltStore struct {
	db *bolt.DB
}

type templateRecord struct {
	ID               string            `json:"id"`
	Origin           string            `json:"origin"`
	BootMode         string            `json:"boot_mode"`
	BootIndexDigest  string            `json:"boot_index_digest"`
	Namespace        string            `json:"namespace"`
	ParentTemplateID string            `json:"parent_template_id,omitempty"`
	SourceSandboxID  string            `json:"source_sandbox_id,omitempty"`
	ImageName        string            `json:"image_name,omitempty"`
	BuildRef         string            `json:"build_ref,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
	CreatedAt        int64             `json:"created_at"`
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

func (s *BoltStore) CreateNetworkSlot(ctx context.Context, rec NetworkSlotRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := networkSlotBucketKey(rec.SlotID)
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal network slot %d: %w", rec.SlotID, err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		slots := tx.Bucket([]byte("network_slots"))
		if slots.Get([]byte(key)) != nil {
			return fmt.Errorf("%w: network slot %d", ErrAlreadyExists, rec.SlotID)
		}
		return slots.Put([]byte(key), data)
	})
}

func networkSlotBucketKey(slotID int) string {
	return strconv.Itoa(slotID)
}

func (s *BoltStore) UpdateNetworkSlot(ctx context.Context, rec NetworkSlotRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := networkSlotBucketKey(rec.SlotID)
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal state record: %w", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		slots := tx.Bucket([]byte("network_slots"))
		existingData := slots.Get([]byte(key))
		if existingData == nil {
			return fmt.Errorf("%w: network slot %d", ErrNotFound, rec.SlotID)
		}
		var existing NetworkSlotRecord
		if err := json.Unmarshal(existingData, &existing); err != nil {
			return fmt.Errorf("unmarshal network slot %d: %w", rec.SlotID, err)
		}
		if rec.SlotID != existing.SlotID {
			return fmt.Errorf("network slot %d identity is immutable: stored slot ID is %d", rec.SlotID, existing.SlotID)
		}
		return slots.Put([]byte(key), data)
	})
}

func (s *BoltStore) GetNetworkSlot(ctx context.Context, slotID int) (NetworkSlotRecord, error) {
	var rec NetworkSlotRecord
	key := networkSlotBucketKey(slotID)
	if err := s.get(ctx, []byte("network_slots"), key, &rec); err != nil {
		return rec, err
	}
	if rec.SlotID != slotID {
		return NetworkSlotRecord{}, fmt.Errorf("network slot bucket key %d contains slot ID %d", slotID, rec.SlotID)
	}
	return rec, nil
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

func (s *BoltStore) DeleteNetworkSlot(ctx context.Context, slotID int) error {
	return s.delete(ctx, []byte("network_slots"), networkSlotBucketKey(slotID))
}

func (s *BoltStore) CreateTemplate(_ context.Context, entry conchtemplate.Entry) error {
	normalized, err := conchtemplate.NormalizeEntry(entry)
	if err != nil {
		return err
	}
	rec := templateRecordFromEntry(normalized)
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal template entry: %w", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		templates := tx.Bucket([]byte("templates"))
		if templates.Get([]byte(normalized.ID)) != nil {
			return fmt.Errorf("%w: %s", conchtemplate.ErrAlreadyExists, normalized.ID)
		}
		return templates.Put([]byte(normalized.ID), data)
	})
}

func (s *BoltStore) GetTemplate(ctx context.Context, id string) (conchtemplate.Entry, error) {
	var rec templateRecord
	if err := s.get(ctx, []byte("templates"), id, &rec); err != nil {
		return conchtemplate.Entry{}, err
	}
	return templateEntryFromRecord(rec)
}

func (s *BoltStore) ListTemplates(ctx context.Context) ([]conchtemplate.Entry, error) {
	var out []conchtemplate.Entry
	err := s.list(ctx, []byte("templates"), func(data []byte) error {
		var rec templateRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		entry, err := templateEntryFromRecord(rec)
		if err != nil {
			return err
		}
		out = append(out, entry)
		return nil
	})
	return out, err
}

func (s *BoltStore) DeleteTemplate(ctx context.Context, id string) error {
	return s.delete(ctx, []byte("templates"), id)
}

// PublishCheckpoint atomically creates a complete checkpoint Template Entry
// and advances the Sandbox checkpoint head. Content publication and validation
// happen before this transaction, so a failed transaction can only leave safe
// orphaned content.
func (s *BoltStore) PublishCheckpoint(_ context.Context, publication CheckpointPublication) error {
	entry, err := conchtemplate.NormalizeEntry(publication.Entry)
	if err != nil {
		return err
	}
	templateID := entry.ID
	sandboxID := strings.TrimSpace(publication.SandboxID)
	if sandboxID == "" {
		return fmt.Errorf("sandbox id is required")
	}
	expectedHeadID := strings.TrimSpace(publication.ExpectedHeadTemplateID)
	if expectedHeadID == "" {
		return fmt.Errorf("expected checkpoint head template id is required")
	}
	expectedHeadDigest := strings.TrimSpace(publication.ExpectedHeadBootIndexDigest)
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
		if templates.Get([]byte(templateID)) != nil {
			return fmt.Errorf("%w: %s", conchtemplate.ErrAlreadyExists, templateID)
		}

		var sandboxRecord SandboxRecord
		if data := sandboxes.Get([]byte(sandboxID)); data == nil {
			return fmt.Errorf("%w: %s", ErrNotFound, sandboxID)
		} else if err := json.Unmarshal(data, &sandboxRecord); err != nil {
			return fmt.Errorf("unmarshal sandbox record %s: %w", sandboxID, err)
		}
		currentHeadID := strings.TrimSpace(sandboxRecord.CheckpointHeadTemplateID)
		if currentHeadID == "" {
			return fmt.Errorf("sandbox %s has no checkpoint head template", sandboxID)
		}
		currentHeadDigest := strings.TrimSpace(sandboxRecord.CheckpointHeadBootIndexDigest)
		if currentHeadDigest == "" {
			return fmt.Errorf("sandbox %s checkpoint head %s has no boot index digest", sandboxID, currentHeadID)
		}
		if currentHeadID != expectedHeadID {
			return fmt.Errorf("sandbox %s checkpoint head template changed from %s to %s", sandboxID, expectedHeadID, currentHeadID)
		}
		if currentHeadDigest != expectedHeadDigest {
			return fmt.Errorf("sandbox %s checkpoint head digest changed from %s to %s", sandboxID, expectedHeadDigest, currentHeadDigest)
		}
		if parentID := entry.ParentTemplateID; parentID != currentHeadID {
			return fmt.Errorf("template %s parent %s does not match sandbox %s checkpoint head %s", templateID, parentID, sandboxID, currentHeadID)
		}
		if sourceID := entry.SourceSandboxID; sourceID != strings.TrimSpace(sandboxRecord.SandboxID) {
			return fmt.Errorf("template %s source sandbox %s does not match %s", templateID, sourceID, sandboxRecord.SandboxID)
		}
		sandboxNamespace := strings.TrimSpace(sandboxRecord.Namespace)
		if sandboxNamespace == "" {
			return fmt.Errorf("sandbox %s has no namespace", sandboxID)
		}
		if entry.Namespace != sandboxNamespace {
			return fmt.Errorf(
				"template %s belongs to namespace %s, not sandbox namespace %s",
				templateID,
				entry.Namespace,
				sandboxNamespace,
			)
		}

		sandboxRecord.CheckpointHeadTemplateID = templateID
		sandboxRecord.CheckpointHeadBootIndexDigest = entry.BootIndexDigest
		sandboxData, err := json.Marshal(sandboxRecord)
		if err != nil {
			return fmt.Errorf("marshal sandbox record: %w", err)
		}
		if err := templates.Put([]byte(templateID), templateData); err != nil {
			return err
		}
		return sandboxes.Put([]byte(sandboxID), sandboxData)
	})
}

func templateRecordFromEntry(entry conchtemplate.Entry) templateRecord {
	return templateRecord{
		ID:               entry.ID,
		Origin:           string(entry.Origin),
		BootMode:         string(entry.BootMode),
		BootIndexDigest:  entry.BootIndexDigest,
		Namespace:        entry.Namespace,
		ParentTemplateID: entry.ParentTemplateID,
		SourceSandboxID:  entry.SourceSandboxID,
		ImageName:        entry.ImageName,
		BuildRef:         entry.BuildRef,
		Labels:           entry.Labels,
		CreatedAt:        entry.CreatedAt,
	}
}

func templateEntryFromRecord(rec templateRecord) (conchtemplate.Entry, error) {
	entry, err := conchtemplate.NormalizeEntry(conchtemplate.Entry{
		ID:               rec.ID,
		Origin:           conchtemplate.Origin(rec.Origin),
		BootMode:         conchtemplate.BootMode(rec.BootMode),
		BootIndexDigest:  rec.BootIndexDigest,
		Namespace:        rec.Namespace,
		ParentTemplateID: rec.ParentTemplateID,
		SourceSandboxID:  rec.SourceSandboxID,
		ImageName:        rec.ImageName,
		BuildRef:         rec.BuildRef,
		Labels:           rec.Labels,
		CreatedAt:        rec.CreatedAt,
	})
	if err != nil {
		return conchtemplate.Entry{}, fmt.Errorf("invalid persisted template %s: %w", rec.ID, err)
	}
	return entry, nil
}
