package recovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/daemon/state"
	runtimeSnapshot "github.com/openeuler/Conch/internal/snapshot"
)

const reconcilePrefix = "reconcile:"

type LeaseClient interface {
	WithRuntimeLease(context.Context, string, string) (context.Context, string, error)
}

type SandboxRehydrator interface {
	Rehydrate([]state.SandboxRecord) (int, map[string]struct{}, error)
	CleanupAssignedWithoutReadySandbox(map[string]struct{}) error
}

type Config struct {
	Store             state.Store
	LeaseClient       LeaseClient
	SandboxRehydrator SandboxRehydrator
	DefaultNamespace  string
}

type Result struct {
	SandboxesChecked        int
	SandboxesDowngraded     int
	ContainersChecked       int
	ContainersDowngraded    int
	SnapshotRuntimesChecked int
	SnapshotRuntimesMarked  int
	ViewSnapshotsChecked    int
	ViewSnapshotsMarked     int
	RuntimeLeasesChecked    int
	LeaseErrors             int
	SnapshotCachesRestored  int
	ViewMountsRestored      int
	ViewAliasesRestored     int
	SandboxesRehydrated     int
	RehydrateErrors         int
	RehydrateError          string
}

func Reconcile(ctx context.Context, cfg Config) (Result, error) {
	if cfg.Store == nil {
		return Result{}, fmt.Errorf("state store is required")
	}
	r := reconciler{cfg: cfg}
	return r.run(ctx)
}

type reconciler struct {
	cfg Config
}

func (r reconciler) run(ctx context.Context) (Result, error) {
	var result Result
	sandboxes, err := r.cfg.Store.ListSandboxes(ctx)
	if err != nil {
		return result, fmt.Errorf("list sandbox state: %w", err)
	}
	containers, err := r.cfg.Store.ListContainers(ctx)
	if err != nil {
		return result, fmt.Errorf("list container state: %w", err)
	}
	snapshotRuntimes, err := r.cfg.Store.ListSnapshotRuntimes(ctx)
	if err != nil {
		return result, fmt.Errorf("list snapshot runtime state: %w", err)
	}
	viewSnapshots, err := r.cfg.Store.ListViewSnapshots(ctx)
	if err != nil {
		return result, fmt.Errorf("list view snapshot state: %w", err)
	}
	viewAliases, err := r.cfg.Store.ListViewAliases(ctx)
	if err != nil {
		return result, fmt.Errorf("list view alias state: %w", err)
	}

	sandboxStates := make(map[string]string, len(sandboxes))
	reconciledSandboxes := make([]state.SandboxRecord, 0, len(sandboxes))
	for _, rec := range sandboxes {
		result.SandboxesChecked++
		rec.Namespace = r.normalizeNamespace(rec.Namespace)
		if rec.LeaseID == "" {
			rec.LeaseID = containerdclient.RuntimeLeaseID(rec.Namespace)
		}
		if ok := r.ensureLease(ctx, rec.Namespace, rec.LeaseID); ok {
			result.RuntimeLeasesChecked++
		} else {
			result.LeaseErrors++
			rec.State = state.SandboxUnknown
			rec.LastError = joinReasons(rec.LastError, "runtime lease could not be ensured")
		}
		if rec.State == state.SandboxReady {
			if reason := verifySandboxRuntime(rec); reason != "" {
				rec.State = state.SandboxNotReady
				rec.LastError = joinReasons(rec.LastError, reason)
				result.SandboxesDowngraded++
			}
		}
		if err := r.cfg.Store.UpsertSandbox(ctx, rec); err != nil {
			return result, fmt.Errorf("upsert reconciled sandbox %s: %w", rec.PodSandboxID, err)
		}
		sandboxStates[rec.PodSandboxID] = rec.State
		reconciledSandboxes = append(reconciledSandboxes, rec)
	}

	for _, rec := range containers {
		result.ContainersChecked++
		if rec.State == state.ContainerRunning {
			if sandboxStates[rec.PodSandboxID] != state.SandboxReady {
				rec.State = state.ContainerUnknown
				rec.LastError = joinReasons(rec.LastError, "parent sandbox is not READY after startup reconcile")
				result.ContainersDowngraded++
				if err := r.cfg.Store.UpsertContainer(ctx, rec); err != nil {
					return result, fmt.Errorf("upsert reconciled container %s: %w", rec.ContainerID, err)
				}
			}
		}
	}

	reconciledSnapshotRuntimes := make([]state.SnapshotRuntimeRecord, 0, len(snapshotRuntimes))
	for _, rec := range snapshotRuntimes {
		result.SnapshotRuntimesChecked++
		rec.Namespace = r.normalizeNamespace(rec.Namespace)
		if rec.LeaseID == "" {
			rec.LeaseID = containerdclient.RuntimeLeaseID(rec.Namespace)
		}
		if ok := r.ensureLease(ctx, rec.Namespace, rec.LeaseID); ok {
			result.RuntimeLeasesChecked++
		} else {
			result.LeaseErrors++
			rec.State = state.SandboxUnknown
			rec.LastError = joinReasons(rec.LastError, "runtime lease could not be ensured")
		}
		if reason := verifySnapshotRuntime(rec); reason != "" {
			rec.State = state.SandboxUnknown
			rec.LastError = joinReasons(rec.LastError, reason)
			result.SnapshotRuntimesMarked++
		}
		if err := r.cfg.Store.UpsertSnapshotRuntime(ctx, rec); err != nil {
			return result, fmt.Errorf("upsert reconciled snapshot runtime %s/%s: %w", rec.Namespace, rec.SandboxID, err)
		}
		reconciledSnapshotRuntimes = append(reconciledSnapshotRuntimes, rec)
	}

	reconciledViewSnapshots := make([]state.ViewSnapshotRecord, 0, len(viewSnapshots))
	for _, rec := range viewSnapshots {
		result.ViewSnapshotsChecked++
		rec.Namespace = r.normalizeNamespace(rec.Namespace)
		if rec.LeaseID == "" {
			rec.LeaseID = containerdclient.RuntimeLeaseID(rec.Namespace)
		}
		if ok := r.ensureLease(ctx, rec.Namespace, rec.LeaseID); ok {
			result.RuntimeLeasesChecked++
		} else {
			result.LeaseErrors++
			rec.State = state.SandboxUnknown
			rec.LastError = joinReasons(rec.LastError, "runtime lease could not be ensured")
		}
		if reason := verifyViewSnapshot(rec); reason != "" {
			rec.State = state.SandboxUnknown
			rec.LastError = joinReasons(rec.LastError, reason)
			result.ViewSnapshotsMarked++
		}
		if err := r.cfg.Store.UpsertViewSnapshot(ctx, rec); err != nil {
			return result, fmt.Errorf("upsert reconciled view snapshot %s/%s: %w", rec.Namespace, rec.ParentSnapshotID, err)
		}
		reconciledViewSnapshots = append(reconciledViewSnapshots, rec)
	}

	if len(reconciledSnapshotRuntimes) > 0 || len(reconciledViewSnapshots) > 0 || len(viewAliases) > 0 {
		if snapshotResult, err := runtimeSnapshot.RehydrateRuntimeState(ctx, reconciledSnapshotRuntimes, reconciledViewSnapshots, viewAliases); err != nil {
			result.SnapshotCachesRestored = snapshotResult.ActiveSnapshots
			result.ViewMountsRestored = snapshotResult.ViewMounts
			result.ViewAliasesRestored = snapshotResult.ViewAliases
			result.RehydrateErrors++
			result.RehydrateError = joinReasons(result.RehydrateError, err.Error())
		} else {
			result.SnapshotCachesRestored = snapshotResult.ActiveSnapshots
			result.ViewMountsRestored = snapshotResult.ViewMounts
			result.ViewAliasesRestored = snapshotResult.ViewAliases
		}
	}
	if r.cfg.SandboxRehydrator != nil {
		count, restoredSandboxIDs, err := r.cfg.SandboxRehydrator.Rehydrate(reconciledSandboxes)
		result.SandboxesRehydrated = count
		var rehydrateErr error
		if err != nil {
			result.RehydrateErrors++
			result.RehydrateError = joinReasons(result.RehydrateError, err.Error())
			rehydrateErr = fmt.Errorf("rehydrate sandboxes: %w", err)
		}
		if err := r.cfg.SandboxRehydrator.CleanupAssignedWithoutReadySandbox(restoredSandboxIDs); err != nil {
			result.RehydrateErrors++
			result.RehydrateError = joinReasons(result.RehydrateError, err.Error())
			return result, errors.Join(rehydrateErr, fmt.Errorf("cleanup stale assigned network slots: %w", err))
		}
		if rehydrateErr != nil {
			return result, rehydrateErr
		}
	}

	return result, nil
}

func (r reconciler) ensureLease(ctx context.Context, namespace, leaseID string) bool {
	if r.cfg.LeaseClient == nil {
		return false
	}
	_, _, err := r.cfg.LeaseClient.WithRuntimeLease(ctx, namespace, leaseID)
	return err == nil
}

func (r reconciler) normalizeNamespace(namespace string) string {
	if ns := strings.TrimSpace(namespace); ns != "" {
		return ns
	}
	if ns := strings.TrimSpace(r.cfg.DefaultNamespace); ns != "" {
		return ns
	}
	return "default"
}

func verifySandboxRuntime(rec state.SandboxRecord) string {
	var reasons []string
	if rec.ConchSandboxID == "" {
		reasons = append(reasons, "missing conch sandbox id")
	}
	if rec.VMMPID == 0 && rec.VMMSocketPath == "" && rec.VsockSocketPath == "" {
		reasons = append(reasons, "sandbox runtime details are missing")
	}
	if rec.VMMPID > 0 && !processAlive(rec.VMMPID) {
		reasons = append(reasons, fmt.Sprintf("vmm pid %d is not alive", rec.VMMPID))
	}
	if rec.VMMSocketPath != "" && !unixSocketReady(rec.VMMSocketPath) {
		reasons = append(reasons, "vmm socket is not reachable")
	}
	if rec.VsockSocketPath != "" && !pathExists(rec.VsockSocketPath) {
		reasons = append(reasons, "vsock socket path is missing")
	}
	for _, item := range []struct {
		name string
		path string
	}{
		{name: "rootfs mount", path: rec.RootfsMount},
		{name: "mem mount", path: rec.MemMount},
		{name: "vm mount", path: rec.VMMount},
	} {
		if item.path != "" && !pathExists(item.path) {
			reasons = append(reasons, item.name+" is missing")
		}
	}
	if len(reasons) == 0 {
		return ""
	}
	return strings.Join(reasons, "; ")
}

func verifySnapshotRuntime(rec state.SnapshotRuntimeRecord) string {
	var reasons []string
	for _, item := range []struct {
		name string
		path string
	}{
		{name: "rootfs mount", path: rec.RootfsMount},
		{name: "mem mount", path: rec.MemMount},
		{name: "vm mount", path: rec.VMMount},
	} {
		if strings.TrimSpace(item.path) == "" {
			reasons = append(reasons, item.name+" is empty")
			continue
		}
		if !pathExists(item.path) {
			reasons = append(reasons, item.name+" is missing")
		}
	}
	if len(reasons) == 0 {
		return ""
	}
	return strings.Join(reasons, "; ")
}

func verifyViewSnapshot(rec state.ViewSnapshotRecord) string {
	if strings.TrimSpace(rec.MountPoint) == "" {
		return "view mount point is empty"
	}
	if !pathExists(rec.MountPoint) {
		return "view mount point is missing"
	}
	return ""
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func unixSocketReady(path string) bool {
	conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func joinReasons(existing, reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return existing
	}
	if !strings.HasPrefix(reason, reconcilePrefix) {
		reason = reconcilePrefix + " " + reason
	}
	if strings.TrimSpace(existing) == "" {
		return reason
	}
	if strings.Contains(existing, reason) {
		return existing
	}
	return existing + "; " + reason
}
