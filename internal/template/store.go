package template

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/openeuler/Conch/internal/daemon/state"
)

// ReadyState is the validated content capability published when a Template
// transitions to READY. BootIndexDigest is the Template's immutable content
// identity; BootMode is a cache derived while validating that Boot Index.
type ReadyState struct {
	BootIndexDigest string
	BootMode        string
	BuildRef        string
}

type CreateRequest struct {
	ID               string
	Origin           string
	Namespace        string
	ParentTemplateID string
	SourceSandboxID  string
	ImageName        string
	BuildRef         string
	Labels           map[string]string
}

type Filter struct {
	Origin    string
	BootMode  string
	Namespace string
	State     string
}

type Store interface {
	Create(context.Context, CreateRequest) (state.TemplateRecord, error)
	MarkReady(context.Context, string, ReadyState) error
	PublishCheckpoint(context.Context, state.CheckpointPublication) error
	MarkFailed(context.Context, string, error) error
	Get(context.Context, string) (state.TemplateRecord, error)
	List(context.Context, Filter) ([]state.TemplateRecord, error)
	Delete(context.Context, string) error
}

type StateStore interface {
	UpsertTemplate(context.Context, state.TemplateRecord) error
	GetTemplate(context.Context, string) (state.TemplateRecord, error)
	ListTemplates(context.Context) ([]state.TemplateRecord, error)
	DeleteTemplate(context.Context, string) error
	PublishCheckpoint(context.Context, state.CheckpointPublication) error
}

type PersistentStore struct {
	store StateStore
	now   func() time.Time
}

func NewStore(store StateStore) *PersistentStore {
	return &PersistentStore{
		store: store,
		now:   time.Now,
	}
}

func (s *PersistentStore) Create(ctx context.Context, req CreateRequest) (state.TemplateRecord, error) {
	if s == nil || s.store == nil {
		return state.TemplateRecord{}, fmt.Errorf("template store is not configured")
	}
	origin := strings.TrimSpace(req.Origin)
	if err := validateOrigin(origin); err != nil {
		return state.TemplateRecord{}, err
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		var err error
		id, err = newID()
		if err != nil {
			return state.TemplateRecord{}, err
		}
	}
	now := s.now().UnixNano()
	rec := state.TemplateRecord{
		ID:               id,
		Origin:           origin,
		Namespace:        normalizeNamespace(req.Namespace),
		State:            state.TemplateCreating,
		ParentTemplateID: strings.TrimSpace(req.ParentTemplateID),
		SourceSandboxID:  strings.TrimSpace(req.SourceSandboxID),
		ImageName:        strings.TrimSpace(req.ImageName),
		BuildRef:         strings.TrimSpace(req.BuildRef),
		Labels:           copyMap(req.Labels),
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := s.store.UpsertTemplate(ctx, rec); err != nil {
		return state.TemplateRecord{}, err
	}
	return rec, nil
}

func (s *PersistentStore) MarkReady(ctx context.Context, id string, ready ReadyState) error {
	if s == nil || s.store == nil {
		return fmt.Errorf("template store is not configured")
	}
	validated, err := validateReadyState(ready)
	if err != nil {
		return err
	}
	rec, err := s.store.GetTemplate(ctx, strings.TrimSpace(id))
	if err != nil {
		return err
	}
	if rec.State != state.TemplateCreating {
		return fmt.Errorf("template %s is %s, want %s", rec.ID, rec.State, state.TemplateCreating)
	}
	rec.BootIndexDigest = validated.BootIndexDigest
	rec.BootMode = validated.BootMode
	if buildRef := strings.TrimSpace(ready.BuildRef); buildRef != "" {
		rec.BuildRef = buildRef
	}
	rec.State = state.TemplateReady
	rec.LastError = ""
	rec.UpdatedAt = s.now().UnixNano()
	return s.store.UpsertTemplate(ctx, rec)
}

// PublishCheckpoint delegates the atomic Template READY/checkpoint-head state
// transition to the persistent state store.
func (s *PersistentStore) PublishCheckpoint(ctx context.Context, publication state.CheckpointPublication) error {
	if s == nil || s.store == nil {
		return fmt.Errorf("template store is not configured")
	}
	validated, err := validateReadyState(ReadyState{
		BootIndexDigest: publication.BootIndexDigest,
		BootMode:        publication.BootMode,
	})
	if err != nil {
		return err
	}
	publication.BootIndexDigest = validated.BootIndexDigest
	publication.BootMode = validated.BootMode
	return s.store.PublishCheckpoint(ctx, publication)
}

func (s *PersistentStore) MarkFailed(ctx context.Context, id string, cause error) error {
	if s == nil || s.store == nil {
		return fmt.Errorf("template store is not configured")
	}
	rec, err := s.store.GetTemplate(ctx, strings.TrimSpace(id))
	if err != nil {
		return err
	}
	if rec.State == state.TemplateReady {
		return fmt.Errorf("template %s is %s", rec.ID, rec.State)
	}
	rec.State = state.TemplateFailed
	rec.LastError = ""
	if cause != nil {
		rec.LastError = cause.Error()
	}
	rec.UpdatedAt = s.now().UnixNano()
	return s.store.UpsertTemplate(ctx, rec)
}

func (s *PersistentStore) Get(ctx context.Context, id string) (state.TemplateRecord, error) {
	if s == nil || s.store == nil {
		return state.TemplateRecord{}, fmt.Errorf("template store is not configured")
	}
	return s.store.GetTemplate(ctx, strings.TrimSpace(id))
}

func (s *PersistentStore) List(ctx context.Context, filter Filter) ([]state.TemplateRecord, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("template store is not configured")
	}
	items, err := s.store.ListTemplates(ctx)
	if err != nil {
		return nil, err
	}
	origin := strings.TrimSpace(filter.Origin)
	bootMode := strings.TrimSpace(filter.BootMode)
	if origin != "" {
		if err := validateOrigin(origin); err != nil {
			return nil, err
		}
	}
	if bootMode != "" && bootMode != state.TemplateBootModeCold && bootMode != state.TemplateBootModeResume {
		return nil, fmt.Errorf("unknown template boot mode %q", bootMode)
	}
	namespace := strings.TrimSpace(filter.Namespace)
	stateFilter := strings.TrimSpace(filter.State)
	out := make([]state.TemplateRecord, 0, len(items))
	for _, item := range items {
		if origin != "" && item.Origin != origin {
			continue
		}
		if bootMode != "" && BootMode(item) != bootMode {
			continue
		}
		if namespace != "" && item.Namespace != namespace {
			continue
		}
		if stateFilter != "" && item.State != stateFilter {
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

func (s *PersistentStore) Delete(ctx context.Context, id string) error {
	if s == nil || s.store == nil {
		return fmt.Errorf("template store is not configured")
	}
	return s.store.DeleteTemplate(ctx, strings.TrimSpace(id))
}

func validateOrigin(origin string) error {
	switch origin {
	case state.TemplateOriginImage, state.TemplateOriginCheckpoint:
		return nil
	default:
		return fmt.Errorf("unknown template origin %q", origin)
	}
}

func newID() (string, error) {
	var data [12]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("generate template id: %w", err)
	}
	return "tmpl_" + hex.EncodeToString(data[:]), nil
}

func BootMode(rec state.TemplateRecord) string {
	switch mode := strings.TrimSpace(rec.BootMode); mode {
	case state.TemplateBootModeCold, state.TemplateBootModeResume:
		return mode
	default:
		return ""
	}
}

func validateReadyState(ready ReadyState) (ReadyState, error) {
	rawDigest := strings.TrimSpace(ready.BootIndexDigest)
	parsed, err := digest.Parse(rawDigest)
	if err != nil {
		return ReadyState{}, fmt.Errorf("invalid boot index digest %q: %w", rawDigest, err)
	}
	mode := strings.TrimSpace(ready.BootMode)
	switch mode {
	case state.TemplateBootModeCold, state.TemplateBootModeResume:
	default:
		return ReadyState{}, fmt.Errorf("unknown template boot mode %q", mode)
	}
	return ReadyState{
		BootIndexDigest: parsed.String(),
		BootMode:        mode,
	}, nil
}

func normalizeNamespace(namespace string) string {
	if ns := strings.TrimSpace(namespace); ns != "" {
		return ns
	}
	return "default"
}

func copyMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
