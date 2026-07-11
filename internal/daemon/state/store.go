package state

import "context"

type Store interface {
	Close() error

	UpsertSandbox(context.Context, SandboxRecord) error
	GetSandbox(context.Context, string) (SandboxRecord, error)
	ListSandboxes(context.Context) ([]SandboxRecord, error)
	DeleteSandbox(context.Context, string) error

	UpsertNetworkSlot(context.Context, NetworkSlotRecord) error
	GetNetworkSlot(context.Context, string) (NetworkSlotRecord, error)
	ListNetworkSlots(context.Context) ([]NetworkSlotRecord, error)
	DeleteNetworkSlot(context.Context, string) error

	UpsertContainer(context.Context, ContainerRecord) error
	GetContainer(context.Context, string) (ContainerRecord, error)
	ListContainers(context.Context) ([]ContainerRecord, error)
	DeleteContainer(context.Context, string) error

	UpsertTemplate(context.Context, TemplateRecord) error
	GetTemplate(context.Context, string) (TemplateRecord, error)
	ListTemplates(context.Context) ([]TemplateRecord, error)
	DeleteTemplate(context.Context, string) error
}
