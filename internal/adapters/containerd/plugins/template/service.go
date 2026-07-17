package template

import (
	"context"
	"fmt"
	"sync"

	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	snapshotSvc "github.com/openeuler/Conch/internal/adapters/containerd/plugins/snapshot"
	"github.com/openeuler/Conch/internal/conchplugins"
	"github.com/openeuler/Conch/internal/daemon/state"
	core "github.com/openeuler/Conch/internal/template"
)

type Service struct {
	store   core.Store
	manager *core.Manager
}

func New(store core.StateStore, snapshots core.SnapshotBackend, bootIndexes core.BootIndexBackend) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("template state store is required")
	}
	if snapshots == nil {
		return nil, fmt.Errorf("snapshot backend is required")
	}
	if bootIndexes == nil {
		return nil, fmt.Errorf("boot index backend is required")
	}
	persistent := core.NewStore(store)
	return &Service{
		store:   persistent,
		manager: core.NewManager(persistent, snapshots, bootIndexes),
	}, nil
}

func (s *Service) Create(ctx context.Context, req core.CreateRequest) (state.TemplateRecord, error) {
	if s == nil || s.store == nil {
		return state.TemplateRecord{}, fmt.Errorf("template service is not configured")
	}
	return s.store.Create(ctx, req)
}

func (s *Service) MarkReady(ctx context.Context, id string, ready core.ReadyState) error {
	if s == nil || s.store == nil {
		return fmt.Errorf("template service is not configured")
	}
	return s.store.MarkReady(ctx, id, ready)
}

func (s *Service) PublishCheckpoint(ctx context.Context, publication state.CheckpointPublication) error {
	if s == nil || s.store == nil {
		return fmt.Errorf("template service is not configured")
	}
	return s.store.PublishCheckpoint(ctx, publication)
}

func (s *Service) MarkFailed(ctx context.Context, id string, cause error) error {
	if s == nil || s.store == nil {
		return fmt.Errorf("template service is not configured")
	}
	return s.store.MarkFailed(ctx, id, cause)
}

func (s *Service) Get(ctx context.Context, id string) (state.TemplateRecord, error) {
	if s == nil || s.store == nil {
		return state.TemplateRecord{}, fmt.Errorf("template service is not configured")
	}
	return s.store.Get(ctx, id)
}

func (s *Service) List(ctx context.Context, filter core.Filter) ([]state.TemplateRecord, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("template service is not configured")
	}
	return s.store.List(ctx, filter)
}

func (s *Service) Delete(ctx context.Context, id string) error {
	if s == nil || s.store == nil {
		return fmt.Errorf("template service is not configured")
	}
	return s.store.Delete(ctx, id)
}

func (s *Service) PrepareSandboxBoot(ctx context.Context, req core.PrepareSandboxBootRequest) (core.PreparedSandboxBoot, error) {
	if s == nil || s.manager == nil {
		return core.PreparedSandboxBoot{}, fmt.Errorf("template boot manager is not configured")
	}
	return s.manager.PrepareSandboxBoot(ctx, req)
}

func (s *Service) ReleaseSandboxBoot(ctx context.Context, req core.ReleaseSandboxBootRequest) error {
	if s == nil || s.manager == nil {
		return fmt.Errorf("template boot manager is not configured")
	}
	return s.manager.ReleaseSandboxBoot(ctx, req)
}

var (
	readyMu    sync.Mutex
	readyCh    chan<- *Service
	stateStore core.StateStore
)

func SetReadyChannel(ch chan<- *Service) {
	readyMu.Lock()
	defer readyMu.Unlock()
	readyCh = ch
}

func SetStateStore(store core.StateStore) {
	readyMu.Lock()
	defer readyMu.Unlock()
	stateStore = store
}

func currentStateStore() core.StateStore {
	readyMu.Lock()
	defer readyMu.Unlock()
	return stateStore
}

func publishReady(svc *Service) {
	readyMu.Lock()
	ch := readyCh
	readyMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- svc:
	default:
	}
}

func init() {
	registry.Register(&plugin.Registration{
		Type:   conchplugins.TemplateServicePluginType,
		ID:     conchplugins.TemplateServiceID,
		Config: &struct{}{},
		Requires: []plugin.Type{
			conchplugins.SnapshotServicePluginType,
			conchplugins.ImageServicePluginType,
		},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			snapshotInst, err := ic.GetByID(conchplugins.SnapshotServicePluginType, conchplugins.SnapshotServiceID)
			if err != nil {
				return nil, err
			}
			snapshotProvider, ok := snapshotInst.(snapshotSvc.ServerProvider)
			if !ok {
				return nil, fmt.Errorf("%s does not provide snapshot server", conchplugins.SnapshotServiceURI)
			}
			imageInst, err := ic.GetByID(conchplugins.ImageServicePluginType, conchplugins.ImageServiceID)
			if err != nil {
				return nil, err
			}
			bootIndexes, ok := imageInst.(core.BootIndexBackend)
			if !ok {
				return nil, fmt.Errorf("%s does not provide boot index resolution", conchplugins.ImageServiceURI)
			}
			svc, err := New(currentStateStore(), snapshotProvider.SnapshotServer(), bootIndexes)
			if err != nil {
				return nil, err
			}
			publishReady(svc)
			return svc, nil
		},
	})
}
