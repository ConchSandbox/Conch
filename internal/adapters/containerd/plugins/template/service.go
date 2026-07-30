package template

import (
	"context"
	"fmt"
	"sync"

	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/openeuler/Conch/internal/conchplugins"
	core "github.com/openeuler/Conch/internal/template"
)

type Service struct {
	store core.Store
}

func New(store core.StateStore) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("template state store is required")
	}
	return &Service{
		store: core.NewStore(store),
	}, nil
}

func (s *Service) Create(ctx context.Context, entry core.Entry) (core.Entry, error) {
	if s == nil || s.store == nil {
		return core.Entry{}, fmt.Errorf("template service is not configured")
	}
	return s.store.Create(ctx, entry)
}

func (s *Service) Get(ctx context.Context, id string) (core.Entry, error) {
	if s == nil || s.store == nil {
		return core.Entry{}, fmt.Errorf("template service is not configured")
	}
	return s.store.Get(ctx, id)
}

func (s *Service) List(ctx context.Context, filter core.Filter) ([]core.Entry, error) {
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
		InitFn: func(ic *plugin.InitContext) (any, error) {
			svc, err := New(currentStateStore())
			if err != nil {
				return nil, err
			}
			publishReady(svc)
			return svc, nil
		},
	})
}
