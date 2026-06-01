package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/openeuler/Conch/internal/daemon/state"
)

type RehydrateResult struct {
	ActiveSnapshots int
	ViewMounts      int
	ViewAliases     int
}

func RehydrateRuntimeState(ctx context.Context, runtimes []state.SnapshotRuntimeRecord, views []state.ViewSnapshotRecord, aliases []state.ViewAliasRecord) (RehydrateResult, error) {
	if gServer == nil || gServer.snt == nil {
		return RehydrateResult{}, fmt.Errorf("server not init")
	}
	return gServer.rehydrateRuntimeState(ctx, runtimes, views, aliases)
}

func (s *server) rehydrateRuntimeState(ctx context.Context, runtimes []state.SnapshotRuntimeRecord, views []state.ViewSnapshotRecord, aliases []state.ViewAliasRecord) (RehydrateResult, error) {
	var (
		result RehydrateResult
		errs   []error
	)
	for _, rec := range runtimes {
		if rec.State != state.SandboxReady {
			continue
		}
		for _, key := range []string{rec.RootfsKey, rec.MemKey} {
			if key == "" {
				continue
			}
			info, err := s.snt.Stat(ctx, rec.Namespace, key)
			if err != nil {
				errs = append(errs, fmt.Errorf("stat active snapshot %s/%s: %w", rec.Namespace, key, err))
				continue
			}
			s.addActiveSnapshot(rec.Namespace, key, &info)
			result.ActiveSnapshots++
		}
	}
	aliasRefs := make(map[string]int, len(aliases))
	for _, rec := range aliases {
		if rec.Namespace == "" || rec.ParentSnapshotID == "" {
			continue
		}
		aliasRefs[rec.Namespace+"/"+rec.ParentSnapshotID]++
	}
	restoredViews := make(map[string]struct{}, len(views))
	for _, rec := range views {
		if rec.State != state.SandboxReady {
			continue
		}
		if rec.MountPoint == "" {
			continue
		}
		if _, err := os.Stat(rec.MountPoint); err != nil {
			errs = append(errs, fmt.Errorf("stat view mount %s/%s: %w", rec.Namespace, rec.MountPoint, err))
			continue
		}
		refCount := rec.RefCount
		if aliasCount := aliasRefs[rec.Namespace+"/"+rec.ParentSnapshotID]; aliasCount > refCount {
			refCount = aliasCount
		}
		s.viewMgr.restoreViewMount(rec.Namespace, rec.ParentSnapshotID, rec.ViewSnapshotKey, rec.MountPoint, refCount)
		restoredViews[rec.Namespace+"/"+rec.ParentSnapshotID] = struct{}{}
		result.ViewMounts++
	}
	for _, rec := range aliases {
		if rec.AliasKey == "" || rec.ParentSnapshotID == "" {
			continue
		}
		if _, ok := restoredViews[rec.Namespace+"/"+rec.ParentSnapshotID]; !ok {
			continue
		}
		s.viewMgr.restoreViewAlias(rec.Namespace, rec.AliasKey, rec.ParentSnapshotID)
		result.ViewAliases++
	}
	return result, errors.Join(errs...)
}
