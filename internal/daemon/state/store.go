package state

import (
	"context"

	"github.com/openeuler/Conch/internal/template"
)

type Store interface {
	Close() error

	UpsertSandbox(context.Context, SandboxRecord) error
	GetSandbox(context.Context, string) (SandboxRecord, error)
	ListSandboxes(context.Context) ([]SandboxRecord, error)
	DeleteSandbox(context.Context, string) error

	CreateTemplate(context.Context, template.Entry) error
	GetTemplate(context.Context, string) (template.Entry, error)
	ListTemplates(context.Context) ([]template.Entry, error)
	DeleteTemplate(context.Context, string) error
	PublishCheckpoint(context.Context, template.Entry) error
}
