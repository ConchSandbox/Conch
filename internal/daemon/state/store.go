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
	PublishCheckpoint(context.Context, template.Entry) error
	BeginTemplateCleanup(context.Context, string) (TemplateCleanupRecord, error)
	GetTemplateCleanup(context.Context, string) (TemplateCleanupRecord, error)
	ListTemplateCleanups(context.Context) ([]TemplateCleanupRecord, error)
	MarkTemplateCleanupArtifactsRemoved(context.Context, string) error
	CompleteTemplateCleanup(context.Context, string) error
}
