package snapshot

import (
	"context"
	"errors"
	"fmt"

	"github.com/openeuler/Conch/internal/daemon/state"
	"github.com/openeuler/Conch/internal/snapshot/common"
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
	aliasKinds := make(map[string]string, len(aliases))
	for _, rec := range aliases {
		if rec.Namespace == "" || rec.ParentSnapshotID == "" {
			continue
		}
		key := rec.Namespace + "/" + rec.ParentSnapshotID
		aliasRefs[key]++
		if rec.MountKind != "" {
			aliasKinds[key] = rec.MountKind
		}
	}
	restoredViews := make(map[string]struct{}, len(views))
	for _, rec := range views {
		if rec.State != state.SandboxReady {
			continue
		}
		if rec.MountPoint == "" {
			continue
		}
		if !IsMountPoint(rec.MountPoint) {
			errs = append(errs, fmt.Errorf("view mount %s/%s is not mounted", rec.Namespace, rec.MountPoint))
			continue
		}
		key := rec.Namespace + "/" + rec.ParentSnapshotID
		snt := s.snt
		if aliasKinds[key] == common.SnapshotMountRootfs && s.rootfsSnt != nil {
			snt = s.rootfsSnt
		}
		mounts, err := snt.Mounts(ctx, rec.Namespace, rec.ViewSnapshotKey)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve view mount %s/%s: %w", rec.Namespace, rec.ViewSnapshotKey, err))
			continue
		}
		if len(mounts) == 0 {
			errs = append(errs, fmt.Errorf("resolve view mount %s/%s: no mount info", rec.Namespace, rec.ViewSnapshotKey))
			continue
		}
		refCount := rec.RefCount
		if aliasCount := aliasRefs[key]; aliasCount > refCount {
			refCount = aliasCount
		}
		s.viewMgr.restoreViewMount(rec.Namespace, rec.ParentSnapshotID, rec.ViewSnapshotKey, rec.MountPoint, refCount, mounts, mountActivationKey("view", rec.Namespace, rec.ViewSnapshotKey))
		restoredViews[key] = struct{}{}
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
