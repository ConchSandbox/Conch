package containerdtemplate

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/labels"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	conchimage "github.com/openeuler/Conch/internal/image"
	conchtemplate "github.com/openeuler/Conch/internal/template"
)

const (
	schemaLabel        = "io.conch.template.schema"
	originLabel        = "io.conch.template.origin"
	parentLabel        = "io.conch.template.parent"
	sourceSandboxLabel = "io.conch.template.source-sandbox"
	sourceRefLabel     = "io.conch.template.source-ref"
	userLabelPrefix    = "io.conch.template.user."
	schemaVersion      = "1"
)

type Store struct {
	images  images.Store
	content content.Store
}

func NewStore(client *containerdclient.Client) *Store {
	if client == nil || client.Client == nil {
		return &Store{}
	}
	return &Store{images: client.ImageService(), content: client.ContentStore()}
}

func (s *Store) Create(ctx context.Context, entry conchtemplate.Entry, target ocispec.Descriptor) (conchtemplate.Entry, error) {
	if err := s.configured(); err != nil {
		return conchtemplate.Entry{}, err
	}
	normalized, err := conchtemplate.NormalizeEntry(entry)
	if err != nil {
		return conchtemplate.Entry{}, err
	}
	if target.Digest.String() != normalized.BootIndexDigest {
		return conchtemplate.Entry{}, conchtemplate.ErrInvalidArtifact.Wrap(fmt.Errorf(
			"boot index target %s does not match Template digest %s", target.Digest, normalized.BootIndexDigest,
		))
	}
	nsctx := containerdclient.NewNamespaceContext(ctx)
	info, err := conchimage.InspectBootIndexContent(nsctx, s.content, target)
	if err != nil {
		return conchtemplate.Entry{}, conchtemplate.ErrInvalidArtifact.Wrap(err)
	}
	kind, bootMode := conchimage.ImageKindBootIndexCold, conchtemplate.BootModeCold
	if info.Resume {
		kind, bootMode = conchimage.ImageKindBootIndexResume, conchtemplate.BootModeResume
	}
	if normalized.BootMode != bootMode {
		return conchtemplate.Entry{}, conchtemplate.ErrInvalidArtifact.Wrap(fmt.Errorf(
			"Template boot mode %q does not match Boot Index mode %q", normalized.BootMode, bootMode,
		))
	}
	name, err := canonicalName(normalized.BootIndexDigest)
	if err != nil {
		return conchtemplate.Entry{}, err
	}
	recordLabels, err := encodeLabels(normalized, kind)
	if err != nil {
		return conchtemplate.Entry{}, err
	}
	labelChildren := images.SetChildrenLabels(s.content, images.ChildrenHandler(s.content))
	if err := images.WalkNotEmpty(nsctx, labelChildren, target); err != nil {
		return conchtemplate.Entry{}, fmt.Errorf("label Template content: %w", err)
	}
	record, err := s.images.Create(nsctx, images.Image{
		Name: name, Target: target, Labels: recordLabels,
	})
	if err != nil {
		return conchtemplate.Entry{}, translateError("create Template image record", err)
	}
	return s.entryFromRecord(nsctx, record)
}

func (s *Store) Get(ctx context.Context, rawDigest string) (conchtemplate.Entry, error) {
	if err := s.configured(); err != nil {
		return conchtemplate.Entry{}, err
	}
	name, err := canonicalName(rawDigest)
	if err != nil {
		return conchtemplate.Entry{}, err
	}
	nsctx := containerdclient.NewNamespaceContext(ctx)
	record, err := s.images.Get(nsctx, name)
	if err != nil {
		return conchtemplate.Entry{}, translateError("get Template image record", err)
	}
	return s.entryFromRecord(nsctx, record)
}

func (s *Store) List(ctx context.Context, filter conchtemplate.Filter) ([]conchtemplate.Entry, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	if err := validateFilter(filter); err != nil {
		return nil, err
	}
	nsctx := containerdclient.NewNamespaceContext(ctx)
	records, err := s.images.List(
		nsctx,
		`labels."`+schemaLabel+`"==`+schemaVersion,
	)
	if err != nil {
		return nil, translateError("list Template image records", err)
	}
	out := make([]conchtemplate.Entry, 0, len(records))
	for _, record := range records {
		if record.Labels[schemaLabel] != schemaVersion {
			continue
		}
		entry, err := s.entryFromRecord(nsctx, record)
		if err != nil {
			return nil, err
		}
		if filter.Origin != "" && entry.Origin != filter.Origin {
			continue
		}
		if filter.BootMode != "" && entry.BootMode != filter.BootMode {
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

func (s *Store) Delete(ctx context.Context, rawDigest string) error {
	if err := s.configured(); err != nil {
		return err
	}
	expected, err := digest.Parse(strings.TrimSpace(rawDigest))
	if err != nil {
		return conchtemplate.ErrInvalidArgument.Wrap(fmt.Errorf("invalid Template ID %q: %w", rawDigest, err))
	}
	name, _ := conchimage.CanonicalTemplateRef(expected.String())
	nsctx := containerdclient.NewNamespaceContext(ctx)
	record, err := s.images.Get(nsctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return translateError("get Template image record", err)
	}
	if record.Labels[schemaLabel] != schemaVersion {
		return nil
	}
	if record.Target.Digest != expected {
		return conchtemplate.ErrFailedPrecondition.Wrap(fmt.Errorf(
			"canonical Template record %s targets %s, want %s", name, record.Target.Digest, expected,
		))
	}
	if err := s.images.Delete(nsctx, name, images.DeleteTarget(&ocispec.Descriptor{Digest: expected})); err != nil && !errdefs.IsNotFound(err) {
		return translateError("delete Template image record", err)
	}
	return nil
}

func (s *Store) entryFromRecord(ctx context.Context, record images.Image) (conchtemplate.Entry, error) {
	if record.Labels[schemaLabel] != schemaVersion {
		return conchtemplate.Entry{}, conchtemplate.ErrNotFound.Wrap(fmt.Errorf("image record %s is not a Template", record.Name))
	}
	wantName, err := canonicalName(record.Target.Digest.String())
	if err != nil || record.Name != wantName {
		return conchtemplate.Entry{}, conchtemplate.ErrInvalidArtifact.Wrap(fmt.Errorf("invalid canonical Template record %s", record.Name))
	}
	info, err := conchimage.InspectBootIndexContent(ctx, s.content, record.Target)
	if err != nil {
		return conchtemplate.Entry{}, conchtemplate.ErrInvalidArtifact.Wrap(err)
	}
	bootMode, wantKind := conchtemplate.BootModeCold, conchimage.ImageKindBootIndexCold
	if info.Resume {
		bootMode, wantKind = conchtemplate.BootModeResume, conchimage.ImageKindBootIndexResume
	}
	if record.Labels[conchimage.ImageKindLabel] != wantKind {
		return conchtemplate.Entry{}, conchtemplate.ErrInvalidArtifact.Wrap(fmt.Errorf("Template image kind does not match Boot Index"))
	}
	entry := conchtemplate.Entry{
		Origin:                conchtemplate.Origin(record.Labels[originLabel]),
		BootMode:              bootMode,
		BootIndexDigest:       record.Target.Digest.String(),
		ParentBootIndexDigest: record.Labels[parentLabel],
		SourceSandboxID:       record.Labels[sourceSandboxLabel],
		SourceRef:             record.Labels[sourceRefLabel],
		CreatedAt:             record.CreatedAt.UnixNano(),
	}
	for key, value := range record.Labels {
		if strings.HasPrefix(key, userLabelPrefix) {
			if entry.Labels == nil {
				entry.Labels = make(map[string]string)
			}
			entry.Labels[strings.TrimPrefix(key, userLabelPrefix)] = value
		}
	}
	return conchtemplate.NormalizeEntry(entry)
}

func (s *Store) configured() error {
	if s == nil || s.images == nil || s.content == nil {
		return fmt.Errorf("template store is not configured")
	}
	return nil
}

func canonicalName(rawDigest string) (string, error) {
	parsed, err := digest.Parse(strings.TrimSpace(rawDigest))
	if err != nil {
		return "", conchtemplate.ErrInvalidArgument.Wrap(fmt.Errorf("invalid Template ID %q: %w", rawDigest, err))
	}
	return conchimage.CanonicalTemplateRef(parsed.String())
}

func encodeLabels(entry conchtemplate.Entry, kind string) (map[string]string, error) {
	out := map[string]string{
		conchimage.ImageKindLabel: kind,
		schemaLabel:               schemaVersion,
		originLabel:               string(entry.Origin),
	}
	for key, value := range map[string]string{
		parentLabel: entry.ParentBootIndexDigest, sourceSandboxLabel: entry.SourceSandboxID, sourceRefLabel: entry.SourceRef,
	} {
		if value != "" {
			out[key] = value
		}
	}
	for key, value := range entry.Labels {
		if strings.TrimSpace(key) == "" {
			return nil, conchtemplate.ErrInvalidArgument.Wrap(fmt.Errorf("Template user label key is empty"))
		}
		out[userLabelPrefix+key] = value
	}
	for key, value := range out {
		if err := labels.Validate(key, value); err != nil {
			return nil, conchtemplate.ErrInvalidArgument.Wrap(err)
		}
	}
	return out, nil
}

func validateFilter(filter conchtemplate.Filter) error {
	if filter.Origin != "" && filter.Origin != conchtemplate.OriginImage && filter.Origin != conchtemplate.OriginCheckpoint {
		return conchtemplate.ErrInvalidArgument.Wrap(fmt.Errorf("unknown template origin %q", filter.Origin))
	}
	if filter.BootMode != "" && filter.BootMode != conchtemplate.BootModeCold && filter.BootMode != conchtemplate.BootModeResume {
		return conchtemplate.ErrInvalidArgument.Wrap(fmt.Errorf("unknown template boot mode %q", filter.BootMode))
	}
	return nil
}

func translateError(action string, err error) error {
	switch {
	case errdefs.IsNotFound(err):
		return conchtemplate.ErrNotFound.Wrap(err)
	case errdefs.IsAlreadyExists(err):
		return conchtemplate.ErrAlreadyExists.Wrap(err)
	case errdefs.IsInvalidArgument(err):
		return conchtemplate.ErrInvalidArgument.Wrap(err)
	default:
		return fmt.Errorf("%s: %w", action, err)
	}
}
