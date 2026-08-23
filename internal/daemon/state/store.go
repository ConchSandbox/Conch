package state

import (
	"context"
)

type Store interface {
	Close() error

	UpsertSandbox(context.Context, SandboxRecord) error
	GetSandbox(context.Context, string) (SandboxRecord, error)
	ListSandboxes(context.Context) ([]SandboxRecord, error)
	DeleteSandbox(context.Context, string) error

	AdvanceCheckpointHead(context.Context, string, string, string) error
}
