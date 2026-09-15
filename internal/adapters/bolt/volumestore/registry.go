// Package volumestore persists volume records in a bbolt database.
package volumestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	bolt "go.etcd.io/bbolt"

	"github.com/openeuler/Conch/internal/volume"
)

var (
	volumesBucket = []byte("volumes")
	namesBucket   = []byte("volume-names")
)

type recordV1 struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Token     string `json:"token"`
	CreatedAt int64  `json:"created_at"`
}

type Store struct {
	db *bolt.DB
}

// Open opens (creating if necessary) the volume registry database.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create volume db directory: %w", err)
	}
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		return nil, fmt.Errorf("open volume db: %w", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, bucket := range [][]byte{volumesBucket, namesBucket} {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return fmt.Errorf("create bucket %s: %w", bucket, err)
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Create(ctx context.Context, record volume.VolumeRecord) error {
	if existing, err := s.GetByName(ctx, record.Name); err == nil && existing.ID != record.ID {
		return volume.ErrAlreadyExists.New()
	} else if err != nil && !errors.Is(err, volume.ErrNotFound) {
		return err
	}
	encoded, err := json.Marshal(recordV1{
		ID: record.ID, Name: record.Name, Token: record.Token, CreatedAt: record.CreatedAt,
	})
	if err != nil {
		return fmt.Errorf("marshal volume record: %w", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		volumes := tx.Bucket(volumesBucket)
		if volumes == nil {
			return fmt.Errorf("volumes bucket missing")
		}
		if err := volumes.Put([]byte(record.ID), encoded); err != nil {
			return err
		}
		names := tx.Bucket(namesBucket)
		if names == nil {
			return fmt.Errorf("volume names bucket missing")
		}
		return names.Put([]byte(record.Name), []byte(record.ID))
	})
}

func (s *Store) Get(ctx context.Context, id string) (volume.VolumeRecord, error) {
	var record recordV1
	err := s.db.View(func(tx *bolt.Tx) error {
		volumes := tx.Bucket(volumesBucket)
		if volumes == nil {
			return fmt.Errorf("volumes bucket missing")
		}
		raw := volumes.Get([]byte(id))
		if raw == nil {
			return volume.ErrNotFound.New()
		}
		return json.Unmarshal(raw, &record)
	})
	if err != nil {
		return volume.VolumeRecord{}, err
	}
	return volume.VolumeRecord{ID: record.ID, Name: record.Name, Token: record.Token, CreatedAt: record.CreatedAt}, nil
}

func (s *Store) GetByName(ctx context.Context, name string) (volume.VolumeRecord, error) {
	var id string
	err := s.db.View(func(tx *bolt.Tx) error {
		names := tx.Bucket(namesBucket)
		if names == nil {
			return fmt.Errorf("volume names bucket missing")
		}
		raw := names.Get([]byte(name))
		if raw == nil {
			return volume.ErrNotFound.New()
		}
		id = string(raw)
		return nil
	})
	if err != nil {
		return volume.VolumeRecord{}, err
	}
	return s.Get(ctx, id)
}

func (s *Store) List(ctx context.Context) ([]volume.VolumeRecord, error) {
	var records []volume.VolumeRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		volumes := tx.Bucket(volumesBucket)
		if volumes == nil {
			return fmt.Errorf("volumes bucket missing")
		}
		return volumes.ForEach(func(_, raw []byte) error {
			var record recordV1
			if err := json.Unmarshal(raw, &record); err != nil {
				return fmt.Errorf("decode volume record: %w", err)
			}
			records = append(records, volume.VolumeRecord{
				ID: record.ID, Name: record.Name, Token: record.Token, CreatedAt: record.CreatedAt,
			})
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	record, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		volumes := tx.Bucket(volumesBucket)
		if volumes == nil {
			return fmt.Errorf("volumes bucket missing")
		}
		if err := volumes.Delete([]byte(id)); err != nil {
			return err
		}
		names := tx.Bucket(namesBucket)
		if names == nil {
			return fmt.Errorf("volume names bucket missing")
		}
		return names.Delete([]byte(record.Name))
	})
}
