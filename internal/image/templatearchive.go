package image

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	annotationTemplateName          = "io.conch.template.name"
	annotationTemplateOrigin        = "io.conch.template.origin"
	annotationTemplateParent        = "io.conch.template.parent"
	annotationTemplateSourceSandbox = "io.conch.template.source-sandbox"
	annotationTemplateSourceRef     = "io.conch.template.source-ref"
	annotationTemplateLabels        = "io.conch.template.labels"

	archiveOriginImage      = "image"
	archiveOriginCheckpoint = "checkpoint"
)

// TemplateArchiveRecord is one Template stored in an offline archive.
// Resume and Target are filled by import. The Boot Index digest is the
// content identity; archive annotations carry the mutable name and metadata.
type TemplateArchiveRecord struct {
	Name                  string
	Origin                string
	BootIndexDigest       string
	ParentBootIndexDigest string
	SourceSandboxID       string
	SourceRef             string
	Labels                map[string]string
	Resume                bool
	Target                ocispec.Descriptor
}

// ExportTemplateArchive writes an OCI image layout tar of each Boot Index and
// every blob it references. Name, origin, lineage, and labels are annotations
// on the layout index, so they do not change the Boot Index digest.
func ExportTemplateArchive(ctx context.Context, store content.Store, w io.Writer, records []TemplateArchiveRecord) error {
	if store == nil {
		return fmt.Errorf("content store is required")
	}
	if w == nil {
		return ErrInvalidArgument.WrapMessage(fmt.Errorf("archive writer is required"), "archive writer is required")
	}
	if len(records) == 0 {
		return ErrInvalidArgument.WrapMessage(fmt.Errorf("at least one template is required"), "at least one template is required")
	}
	opts := []archive.ExportOpt{archive.WithSkipDockerManifest()}
	seen := make(map[string]struct{}, len(records))
	for _, rec := range records {
		name := strings.TrimSpace(rec.Name)
		if name == "" {
			return ErrInvalidArgument.WrapMessage(fmt.Errorf("template name is required"), "template name is required")
		}
		if _, ok := seen[name]; ok {
			return ErrInvalidArgument.WrapMessage(
				fmt.Errorf("duplicate template name %s", name),
				fmt.Sprintf("duplicate template name %s", name),
			)
		}
		seen[name] = struct{}{}
		desc, info, err := inspectBootIndexByDigest(ctx, store, rec.BootIndexDigest)
		if err != nil {
			if errdefs.IsNotFound(err) {
				return ErrNotFound.WrapMessage(err, fmt.Sprintf("template %s boot index is missing", name))
			}
			return fmt.Errorf("template %s: %w", name, err)
		}
		origin, err := archiveOrigin(rec.Origin, info.Resume, false)
		if err != nil {
			return ErrInvalidArgument.WrapMessage(err, err.Error())
		}
		rec.Origin = origin
		annotations, err := templateArchiveAnnotations(rec)
		if err != nil {
			return err
		}
		desc.Annotations = annotations
		opts = append(opts, archive.WithManifest(desc, name))
	}
	if err := archive.Export(ctx, store, w, opts...); err != nil {
		return fmt.Errorf("write template archive: %w", err)
	}
	return nil
}

// ImportTemplateArchive loads an OCI layout tar, or a gzip or zstd compressed
// tar, into store. Each returned record is a validated Boot Index plus the
// Template metadata carried by the archive.
func ImportTemplateArchive(ctx context.Context, store content.Store, r io.Reader) ([]TemplateArchiveRecord, error) {
	if store == nil {
		return nil, fmt.Errorf("content store is required")
	}
	if r == nil {
		return nil, ErrInvalidArgument.WrapMessage(fmt.Errorf("archive reader is required"), "archive reader is required")
	}
	decompressed, err := compression.DecompressStream(r)
	if err != nil {
		return nil, ErrInvalidContent.WrapMessage(err, "template archive compression is not supported")
	}
	defer decompressed.Close()

	indexDesc, err := archive.ImportIndex(ctx, store, decompressed)
	if err != nil {
		return nil, ErrInvalidContent.WrapMessage(err, "template archive is not a valid OCI or Docker image layout")
	}
	raw, err := content.ReadBlob(ctx, store, indexDesc)
	if err != nil {
		return nil, ErrInvalidContent.WrapMessage(err, "template archive index is unreadable")
	}
	var index ocispec.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		return nil, ErrInvalidContent.WrapMessage(err, "template archive index is invalid")
	}
	if len(index.Manifests) == 0 {
		return nil, ErrInvalidContent.WrapMessage(fmt.Errorf("archive index is empty"), "template archive contains no templates")
	}

	records := make([]TemplateArchiveRecord, 0, len(index.Manifests))
	seen := make(map[string]struct{}, len(index.Manifests))
	for _, manifest := range index.Manifests {
		record, err := templateRecordFromManifest(ctx, store, manifest)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[record.Name]; ok {
			return nil, ErrInvalidContent.WrapMessage(
				fmt.Errorf("duplicate template name %s", record.Name),
				fmt.Sprintf("duplicate template name %s", record.Name),
			)
		}
		seen[record.Name] = struct{}{}
		records = append(records, record)
	}
	if len(records) == 0 {
		return nil, ErrInvalidContent.WrapMessage(fmt.Errorf("archive has no template manifests"), "template archive contains no templates")
	}
	return records, nil
}

func templateRecordFromManifest(ctx context.Context, store content.Store, manifest ocispec.Descriptor) (TemplateArchiveRecord, error) {
	if err := manifest.Digest.Validate(); err != nil {
		return TemplateArchiveRecord{}, ErrInvalidContent.WrapMessage(err, "template archive entry digest is invalid")
	}
	if manifest.MediaType == "" {
		manifest.MediaType = ocispec.MediaTypeImageIndex
	}
	if manifest.MediaType != ocispec.MediaTypeImageIndex {
		return TemplateArchiveRecord{}, ErrInvalidContent.WrapMessage(
			fmt.Errorf("media type %s", manifest.MediaType),
			fmt.Sprintf("template archive entry %s is not a Conch Boot Index", manifest.Digest),
		)
	}
	info, err := InspectBootIndexContent(ctx, store, manifest)
	if err != nil {
		return TemplateArchiveRecord{}, ErrInvalidContent.WrapMessage(
			err,
			fmt.Sprintf("template archive entry %s is not a Conch Boot Index", manifest.Digest),
		)
	}
	if manifest.Size <= 0 {
		contentInfo, err := store.Info(ctx, manifest.Digest)
		if err != nil {
			return TemplateArchiveRecord{}, ErrInvalidContent.WrapMessage(err, "template archive boot index is unreadable")
		}
		manifest.Size = contentInfo.Size
	}
	record, err := recordFromArchiveAnnotations(manifest.Annotations, info)
	if err != nil {
		return TemplateArchiveRecord{}, err
	}
	record.Target = ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageIndex,
		Digest:    manifest.Digest,
		Size:      manifest.Size,
	}
	return record, nil
}

func templateArchiveAnnotations(rec TemplateArchiveRecord) (map[string]string, error) {
	out := map[string]string{
		annotationTemplateName:   strings.TrimSpace(rec.Name),
		annotationTemplateOrigin: rec.Origin,
	}
	if parent := strings.TrimSpace(rec.ParentBootIndexDigest); parent != "" {
		parsed, err := digest.Parse(parent)
		if err != nil {
			return nil, ErrInvalidArgument.WrapMessage(err, "template parent boot index digest is invalid")
		}
		out[annotationTemplateParent] = parsed.String()
	}
	if value := strings.TrimSpace(rec.SourceSandboxID); value != "" {
		out[annotationTemplateSourceSandbox] = value
	}
	if value := strings.TrimSpace(rec.SourceRef); value != "" {
		out[annotationTemplateSourceRef] = value
	}
	if len(rec.Labels) > 0 {
		encoded, err := json.Marshal(rec.Labels)
		if err != nil {
			return nil, ErrInvalidArgument.WrapMessage(err, "template labels are invalid")
		}
		out[annotationTemplateLabels] = string(encoded)
	}
	return out, nil
}

func recordFromArchiveAnnotations(annotations map[string]string, info BootIndexInfo) (TemplateArchiveRecord, error) {
	name := strings.TrimSpace(annotations[annotationTemplateName])
	if name == "" {
		return TemplateArchiveRecord{}, ErrInvalidContent.WrapMessage(
			fmt.Errorf("boot index %s has no template name", info.BootIndexDigest),
			fmt.Sprintf("template archive entry %s has no template name", info.BootIndexDigest),
		)
	}
	origin, err := archiveOrigin(annotations[annotationTemplateOrigin], info.Resume, true)
	if err != nil {
		return TemplateArchiveRecord{}, ErrInvalidContent.WrapMessage(err, err.Error())
	}
	parent := strings.TrimSpace(annotations[annotationTemplateParent])
	if parent != "" {
		parsed, err := digest.Parse(parent)
		if err != nil {
			return TemplateArchiveRecord{}, ErrInvalidContent.WrapMessage(err, "template archive parent digest is invalid")
		}
		parent = parsed.String()
	}
	labels, err := decodeArchiveLabels(annotations[annotationTemplateLabels])
	if err != nil {
		return TemplateArchiveRecord{}, err
	}
	dgst, err := digest.Parse(info.BootIndexDigest)
	if err != nil {
		return TemplateArchiveRecord{}, ErrInvalidContent.WrapMessage(err, "template boot index digest is invalid")
	}
	return TemplateArchiveRecord{
		Name:                  name,
		Origin:                origin,
		BootIndexDigest:       dgst.String(),
		ParentBootIndexDigest: parent,
		SourceSandboxID:       strings.TrimSpace(annotations[annotationTemplateSourceSandbox]),
		SourceRef:             strings.TrimSpace(annotations[annotationTemplateSourceRef]),
		Labels:                labels,
		Resume:                info.Resume,
	}, nil
}

func archiveOrigin(origin string, resume bool, inferEmpty bool) (string, error) {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		if !inferEmpty {
			return "", fmt.Errorf("unknown template origin %q", origin)
		}
		if resume {
			return archiveOriginCheckpoint, nil
		}
		return archiveOriginImage, nil
	}
	switch origin {
	case archiveOriginImage:
		if resume {
			return "", fmt.Errorf("template origin image does not match a resume Boot Index")
		}
	case archiveOriginCheckpoint:
		if !resume {
			return "", fmt.Errorf("template origin checkpoint requires a resume Boot Index")
		}
	default:
		return "", fmt.Errorf("unknown template origin %q", origin)
	}
	return origin, nil
}

func decodeArchiveLabels(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(raw), &labels); err != nil {
		return nil, ErrInvalidContent.WrapMessage(err, "template archive labels are invalid")
	}
	if len(labels) == 0 {
		return nil, nil
	}
	for key := range labels {
		if strings.TrimSpace(key) == "" {
			return nil, ErrInvalidContent.WrapMessage(fmt.Errorf("empty label key"), "template label key is empty")
		}
	}
	return labels, nil
}
