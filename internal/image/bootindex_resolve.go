package image

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/pkg/ulog"
)

type ResolvedBoot struct {
	BootIndexDigest string
	RootfsKey       string
	MemKey          string
	VMKey           string
	Resume          bool
	VMMName         string
	MemorySizeMB    int64
	// PreGateRequired is true only when the memory component is exposed before
	// its full backing has finished materializing. Normal local unpack leaves it
	// false.
	PreGateRequired         bool
	ExternalMemoryErofsPath string
	PreGateProfile          []byte
	MaterializeCritical     func(context.Context, int64, []uint64) error
	MaterializeAll          func(context.Context) error
	MaterializeCommit       func() error
}

func (r ResolvedBoot) ExternalMemoryErofsPathOK() bool {
	path := strings.TrimSpace(r.ExternalMemoryErofsPath)
	if path == "" {
		return true
	}
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

type LazyResolveOptions struct {
	Reference string
	PlainHTTP bool
	StateDir  string
}

// ResolveBoot validates a Boot Index by digest and idempotently unpacks its
// components into the committed snapshot parents required by Sandbox.
func ResolveBoot(ctx context.Context, client *containerdclient.Client, bootIndexDigest string) (ResolvedBoot, error) {
	resolveCtx, info, err := inspectBootIndex(ctx, client, bootIndexDigest)
	if err != nil {
		return ResolvedBoot{}, err
	}
	snapshotMap, err := unpackBootIndexComponents(resolveCtx, client.Client, info)
	if err != nil {
		return ResolvedBoot{}, fmt.Errorf("unpack boot index %s: %w", info.BootIndexDigest, err)
	}
	result := ResolvedBoot{
		BootIndexDigest: info.BootIndexDigest,
		RootfsKey:       snapshotMap[KindRootfs],
		MemKey:          snapshotMap[KindMemSnapshot],
		VMKey:           snapshotMap[KindSandbox],
		Resume:          info.Resume,
		VMMName:         info.VMMName,
		MemorySizeMB:    info.MemorySizeMB,
	}
	if result.RootfsKey == "" || result.VMKey == "" {
		return ResolvedBoot{}, fmt.Errorf("boot index %s unpack returned incomplete component keys", info.BootIndexDigest)
	}
	if result.Resume && result.MemKey == "" {
		return ResolvedBoot{}, fmt.Errorf("resume boot index %s unpack returned an empty mem snapshot key", info.BootIndexDigest)
	}
	return result, nil
}

func ResolveBootLazy(ctx context.Context, client *containerdclient.Client, bootIndexDigest string, opts LazyResolveOptions) (ResolvedBoot, error) {
	resolveCtx := containerdclient.NewNamespaceContext(ctx)
	_, info, err := inspectBootIndexMetadataByDigest(resolveCtx, client.ContentStore(), bootIndexDigest)
	if err != nil {
		return ResolvedBoot{}, err
	}
	metadata, err := lazyMemoryMetadata(resolveCtx, client.ContentStore(), info)
	if err != nil {
		return ResolvedBoot{}, err
	}
	components, err := unpackBootIndexComponentsWithoutMemory(resolveCtx, client.Client, info)
	if err != nil {
		return ResolvedBoot{}, err
	}
	materializer, err := NewLazyMemoryMaterializer(resolveCtx, opts.Reference, opts.PlainHTTP, opts.StateDir, metadata)
	if err != nil {
		return ResolvedBoot{}, err
	}
	result := ResolvedBoot{
		BootIndexDigest:         info.BootIndexDigest,
		RootfsKey:               components[KindRootfs],
		VMKey:                   components[KindSandbox],
		MemKey:                  "lazy:" + metadata.Layer.Digest.String(),
		Resume:                  true,
		VMMName:                 info.VMMName,
		MemorySizeMB:            info.MemorySizeMB,
		PreGateRequired:         !materializer.Complete(),
		ExternalMemoryErofsPath: materializer.Path(),
		PreGateProfile:          materializer.Profile(),
		MaterializeAll:          materializer.MaterializeAll,
		MaterializeCommit:       materializer.Commit,
		// MaterializeOffsets is safe to expose unconditionally: it no-ops on an
		// empty offset set or an already-complete layer, and configurePreGate
		// supplies the offsets from the locally learned profile when the
		// registry did not carry one. Without this, restores against a sparse
		// file would depend on the full materialization winning the race
		// against the restore's critical-page reads.
		MaterializeCritical: materializer.MaterializeOffsets,
	}
	return result, nil
}

// EnsureLazyMemoryContent promotes a fully materialized lazy backing file into
// containerd only when an operation needs the complete OCI descriptor closure.
func EnsureLazyMemoryContent(ctx context.Context, client *containerdclient.Client, bootIndexDigest, stateDir string) error {
	if client == nil || client.Client == nil {
		return fmt.Errorf("containerd client is required")
	}
	resolveCtx := containerdclient.NewNamespaceContext(ctx)
	_, info, err := inspectBootIndexMetadataByDigest(resolveCtx, client.ContentStore(), bootIndexDigest)
	if err != nil {
		return fmt.Errorf("inspect lazy boot index metadata: %w", err)
	}
	if !info.Resume {
		return nil
	}
	metadata, err := lazyMemoryMetadata(resolveCtx, client.ContentStore(), info)
	if err != nil {
		return fmt.Errorf("resolve lazy memory metadata: %w", err)
	}
	if _, err := client.ContentStore().Info(resolveCtx, metadata.Layer.Digest); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("inspect lazy memory content %s: %w", metadata.Layer.Digest, err)
	}
	started := time.Now()
	if err := commitLazyMemoryLayer(resolveCtx, client.ContentStore(), stateDir, metadata); err != nil {
		return err
	}
	ulog.GetLogger().Info("Promoted lazy memory layer to content store",
		ulog.F("digest", metadata.Layer.Digest),
		ulog.F("bytes", metadata.Layer.Size),
		ulog.F("elapsed", time.Since(started)))
	return nil
}
