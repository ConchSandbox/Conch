package snapshot

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"

	"github.com/openeuler/Conch/internal/runtimeapi"
)

type snapshotRemover interface {
	Remove(context.Context, string) error
}

func (s *Server) List(ctx context.Context, opts runtimeapi.ListSnapshotsOptions) ([]runtimeapi.SnapshotRecord, error) {
	if s == nil || s.snt == nil {
		return nil, fmt.Errorf("snapshot server is not configured")
	}
	items := make(map[string]*snapshots.Info)
	if err := s.snt.List(ctx, items, opts.Filters...); err != nil {
		if errdefs.IsInvalidArgument(err) {
			return nil, ErrInvalidArgument.Wrap(err)
		}
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	out := make([]runtimeapi.SnapshotRecord, 0, len(items))
	for _, info := range items {
		if info != nil {
			out = append(out, snapshotRecord(*info))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Key < out[j].Key
	})
	return out, nil
}

func (s *Server) Remove(ctx context.Context, opts runtimeapi.RemoveSnapshotOptions) error {
	if s == nil || s.snt == nil {
		return fmt.Errorf("snapshot server is not configured")
	}
	if strings.TrimSpace(opts.Key) == "" {
		return ErrInvalidArgument.Wrap(fmt.Errorf("key is required"))
	}
	if err := removeSnapshotKey(ctx, s.snt, opts.Key); err != nil {
		return fmt.Errorf("remove snapshot %s: %w", opts.Key, err)
	}
	return nil
}

func removeSnapshotKey(ctx context.Context, snapshotter snapshotRemover, key string) error {
	if strings.TrimSpace(key) == "" {
		return nil
	}
	if err := snapshotter.Remove(ctx, key); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

func (s *Server) Info(ctx context.Context, opts runtimeapi.SnapshotInfoOptions) (runtimeapi.SnapshotRecord, error) {
	if s == nil || s.snt == nil {
		return runtimeapi.SnapshotRecord{}, fmt.Errorf("snapshot server is not configured")
	}
	if opts.Key == "" {
		return runtimeapi.SnapshotRecord{}, ErrInvalidArgument.Wrap(fmt.Errorf("key is required"))
	}

	stat, err := s.snt.Stat(ctx, opts.Key)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return runtimeapi.SnapshotRecord{}, ErrNotFound.Wrap(err)
		}
		return runtimeapi.SnapshotRecord{}, fmt.Errorf("stat failed for key %s: %w", opts.Key, err)
	}

	mounts, err := s.snt.Mounts(ctx, opts.Key)
	if err != nil || len(mounts) == 0 || len(mounts[0].Options) == 0 {
		viewID := fmt.Sprintf("tmp-v-%d-%s", time.Now().UnixNano(), opts.Key)
		mounts, err = s.snt.View(ctx, viewID, opts.Key)
		if err != nil {
			if errdefs.IsNotFound(err) {
				return runtimeapi.SnapshotRecord{}, ErrNotFound.Wrap(err)
			}
			if errdefs.IsFailedPrecondition(err) {
				return runtimeapi.SnapshotRecord{}, ErrFailedPrecondition.Wrap(err)
			}
			return runtimeapi.SnapshotRecord{}, fmt.Errorf("failed to resolve storage path via mounts or view: %w", err)
		}
		defer s.snt.Remove(ctx, viewID)
	}

	storagePath := ""
	if len(mounts) > 0 {
		for _, opt := range mounts[0].Options {
			if strings.HasPrefix(opt, "upperdir=") {
				storagePath = strings.TrimPrefix(opt, "upperdir=")
				break
			}
			if strings.HasPrefix(opt, "lowerdir=") && storagePath == "" {
				storagePath = strings.Split(strings.TrimPrefix(opt, "lowerdir="), ":")[0]
			}
		}
		if storagePath == "" || storagePath == "overlay" {
			storagePath = mounts[0].Source
		}
	}

	record := snapshotRecord(stat)
	record.StoragePath = storagePath
	return record, nil
}

func snapshotRecord(info snapshots.Info) runtimeapi.SnapshotRecord {
	record := runtimeapi.SnapshotRecord{
		Key:       info.Name,
		Kind:      strings.ToLower(info.Kind.String()),
		Parent:    info.Parent,
		Labels:    info.Labels,
		CreatedAt: info.Created,
		UpdatedAt: info.Updated,
	}
	return record
}
