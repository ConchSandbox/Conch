package client

import (
	"context"

	"github.com/openeuler/Conch/internal/runtimeapi"
)

const (
	listSnapshots  = "/api/snapshot/list"
	removeSnapshot = "/api/snapshot/remove"
)

type ListSnapshotsRequest struct {
	Filters []string `json:"filters,omitempty"`
}

type RemoveSnapshotRequest struct {
	Key string `json:"key"`
}

type SnapshotRecord = runtimeapi.SnapshotRecord

type listSnapshotsResponse struct {
	Snapshots []SnapshotRecord `json:"snapshots"`
}

func (c *Client) ListSnapshots(ctx context.Context, req ListSnapshotsRequest) ([]SnapshotRecord, error) {
	var resp listSnapshotsResponse
	if err := c.postJSON(ctx, listSnapshots, req, &resp); err != nil {
		return nil, err
	}
	return resp.Snapshots, nil
}

func (c *Client) RemoveSnapshot(ctx context.Context, req RemoveSnapshotRequest) error {
	return c.postJSON(ctx, removeSnapshot, req, nil)
}
