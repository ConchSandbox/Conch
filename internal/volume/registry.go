package volume

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/openeuler/Conch/internal/runtimeapi"
)

// VolumeRecord is the persisted form of a named node-local volume.
type VolumeRecord struct {
	ID        string
	Name      string
	Token     string
	CreatedAt int64
}

// Store persists volume records. Implementations must be safe for concurrent
// use.
type Store interface {
	Create(ctx context.Context, record VolumeRecord) error
	Get(ctx context.Context, id string) (VolumeRecord, error)
	GetByName(ctx context.Context, name string) (VolumeRecord, error)
	List(ctx context.Context) ([]VolumeRecord, error)
	Delete(ctx context.Context, id string) error
}

// Registry pairs a persistent Store with the host data directory backing each
// volume. Data directories are named by volume ID under dataDir.
type Registry struct {
	store   Store
	dataDir string
}

func NewRegistry(store Store, dataDir string) (*Registry, error) {
	if store == nil {
		return nil, fmt.Errorf("volume store is required")
	}
	if !filepath.IsAbs(dataDir) {
		return nil, fmt.Errorf("volume data dir must be absolute: %s", dataDir)
	}
	return &Registry{store: store, dataDir: dataDir}, nil
}

// DataDir returns the host directory backing the volume.
func (r *Registry) DataDir(record VolumeRecord) string {
	return filepath.Join(r.dataDir, record.ID)
}

// Create registers a named volume and creates its host data directory. The
// returned token has no consumer yet; it is stored for API compatibility.
func (r *Registry) Create(ctx context.Context, name string) (VolumeRecord, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 255 {
		return VolumeRecord{}, ErrInvalidMount.Wrap(fmt.Errorf("volume name must be 1-255 characters"))
	}
	var idData [16]byte
	if _, err := rand.Read(idData[:]); err != nil {
		return VolumeRecord{}, fmt.Errorf("generate volume id: %w", err)
	}
	var tokenData [16]byte
	if _, err := rand.Read(tokenData[:]); err != nil {
		return VolumeRecord{}, fmt.Errorf("generate volume token: %w", err)
	}
	record := VolumeRecord{
		ID:        hex.EncodeToString(idData[:]),
		Name:      name,
		Token:     hex.EncodeToString(tokenData[:]),
		CreatedAt: time.Now().UnixNano(),
	}
	if err := r.store.Create(ctx, record); err != nil {
		return VolumeRecord{}, err
	}
	if err := mkdirAll(r.DataDir(record)); err != nil {
		return VolumeRecord{}, err
	}
	return record, nil
}

func (r *Registry) Get(ctx context.Context, id string) (VolumeRecord, error) {
	return r.store.Get(ctx, id)
}

func (r *Registry) List(ctx context.Context) ([]VolumeRecord, error) {
	return r.store.List(ctx)
}

// Delete removes a volume record and its host data directory. inUse reports
// whether any sandbox still mounts the volume.
func (r *Registry) Delete(ctx context.Context, id string, inUse func(volumeID string) bool) error {
	record, err := r.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if inUse != nil && inUse(record.ID) {
		return ErrInUse.New()
	}
	if err := r.store.Delete(ctx, id); err != nil {
		return err
	}
	return removeAll(r.DataDir(record))
}

// ResolveMounts turns named E2B volume mounts into host-backed specs for the
// existing virtiofs pipeline. Unknown names fail the whole create request.
func (r *Registry) ResolveMounts(ctx context.Context, mounts []runtimeapi.VolumeMountSpec) ([]runtimeapi.VolumeMountSpec, error) {
	resolved := make([]runtimeapi.VolumeMountSpec, 0, len(mounts))
	for _, mount := range mounts {
		name := strings.TrimSpace(mount.Name)
		if name == "" {
			return nil, ErrInvalidMount.Wrap(fmt.Errorf("volume mount name is required"))
		}
		record, err := r.store.GetByName(ctx, name)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, ErrInvalidMount.Wrap(fmt.Errorf("unknown volume %q", name))
			}
			return nil, err
		}
		resolved = append(resolved, runtimeapi.VolumeMountSpec{
			Name:   record.Name,
			Path:   mount.Path,
			Source: r.DataDir(record),
		})
	}
	return resolved, nil
}
