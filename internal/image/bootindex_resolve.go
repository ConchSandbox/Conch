package image

import (
	"context"
	"fmt"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
)

type ResolvedBoot struct {
	BootIndexDigest string
	RootfsKey       string
	MemKey          string
	VMKey           string
	Resume          bool
	VMMName         string
	MemorySizeMB    int64
	MemoryFormat    string
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
		MemoryFormat:    info.MemoryFormat,
	}
	if result.RootfsKey == "" || result.VMKey == "" {
		return ResolvedBoot{}, fmt.Errorf("boot index %s unpack returned incomplete component keys", info.BootIndexDigest)
	}
	if result.Resume && result.MemKey == "" {
		return ResolvedBoot{}, fmt.Errorf("resume boot index %s unpack returned an empty mem snapshot key", info.BootIndexDigest)
	}
	return result, nil
}
