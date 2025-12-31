package snapshot

import (
	"context"
	"fmt"
)

type Kind uint8

const (
	KindUnknown Kind = iota
	KindRW
	KindSnapshot
	KindImage
)

type SnapshotConfig struct {
	RootDir string // dir which save vm snapshot
	MemSize int64  // memory size of vm, unit is mb

	Rootfs     string // dir which mount overlayfs
	RootfsSock string

	Labels map[string]string // other labels add into info
}
type Opt func(info *SnapshotConfig) error

func Prepare(ctx context.Context, namespace, key, parent string, opts ...Opt) (*SnapshotConfig, error) {
	if gServer.snt == nil {
		return nil, fmt.Errorf("server not init")
	}
	return gServer.Prepare(ctx, namespace, key, parent, opts...)
}

func Commit(ctx context.Context, namespace, name, key string, opts ...Opt) error {
	if gServer.snt == nil {
		return fmt.Errorf("server not init")
	}
	return gServer.Commit(ctx, namespace, name, key, opts...)
}

func Remove(ctx context.Context, namespace, key string) error {
	if gServer.snt == nil {
		return fmt.Errorf("server not init")
	}
	return gServer.Remove(ctx, namespace, key)
}
