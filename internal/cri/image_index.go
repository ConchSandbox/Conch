package cri

import (
	"sort"
	"strings"

	"github.com/openeuler/Conch/internal/runtimeapi"
)

type imageView struct {
	ID           string
	RepoTags     []string
	RepoDigests  []string
	TargetMedia  string
	Size         int64
	Kind         string
	Kinds        []string
	Components   []string
	Labels       map[string]string
	Records      []runtimeapi.ImageRecord
	OriginalRefs []string
	Ready        bool
}

type imageIndex struct {
	views []*imageView
}

func newImageIndex(records []runtimeapi.ImageRecord) *imageIndex {
	byID := make(map[string]*imageView)
	order := make([]string, 0, len(records))
	for _, record := range records {
		id := strings.TrimSpace(record.TargetDigest)
		if id == "" {
			id = strings.TrimSpace(record.Name)
		}
		if id == "" {
			continue
		}
		kind := normalizeImageKind(record)
		view, ok := byID[id]
		if !ok {
			view = &imageView{
				ID:          id,
				TargetMedia: record.TargetMediaType,
				Size:        record.Size,
				Kind:        kind,
				Components:  structureComponents(kind),
				Labels:      cloneMap(record.Labels),
				Ready:       imageReady(record),
			}
			if kind != "" {
				view.Kinds = append(view.Kinds, kind)
			}
			byID[id] = view
			order = append(order, id)
		}
		view.Records = append(view.Records, record)
		if record.TargetMediaType != "" {
			view.TargetMedia = record.TargetMediaType
		}
		if record.Size > view.Size {
			view.Size = record.Size
		}
		if view.Kind == "" && kind != "" {
			view.Kind = kind
			view.Components = structureComponents(kind)
		}
		if kind != "" {
			view.Kinds = appendIfMissing(view.Kinds, kind)
		}
		if len(view.Labels) == 0 && len(record.Labels) > 0 {
			view.Labels = cloneMap(record.Labels)
		}
		if record.Name != "" {
			view.OriginalRefs = appendIfMissing(view.OriginalRefs, record.Name)
			view.RepoTags = appendIfMissing(view.RepoTags, record.Name)
		}
		for _, repoDigest := range record.RepoDigests {
			if repoDigest != "" {
				view.RepoDigests = appendIfMissing(view.RepoDigests, repoDigest)
			}
		}
		view.Ready = view.Ready || imageReady(record)
	}

	views := make([]*imageView, 0, len(order))
	for _, id := range order {
		view := byID[id]
		sort.Strings(view.Kinds)
		sort.Strings(view.Components)
		sort.Strings(view.RepoTags)
		sort.Strings(view.RepoDigests)
		sort.Strings(view.OriginalRefs)
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool {
		if len(views[i].RepoTags) > 0 && len(views[j].RepoTags) > 0 {
			return views[i].RepoTags[0] < views[j].RepoTags[0]
		}
		return views[i].ID < views[j].ID
	})
	return &imageIndex{views: views}
}

func (idx *imageIndex) resolve(ref string) *imageView {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	for _, view := range idx.views {
		if ref == view.ID {
			return view
		}
		for _, tag := range view.RepoTags {
			if ref == tag {
				return view
			}
		}
		for _, repoDigest := range view.RepoDigests {
			if ref == repoDigest {
				return view
			}
		}
	}
	return nil
}

func (idx *imageIndex) list(filter string) []*imageView {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return idx.views
	}
	var out []*imageView
	for _, view := range idx.views {
		if idx.matchesView(view, filter) {
			out = append(out, view)
		}
	}
	return out
}

func (idx *imageIndex) matchesView(view *imageView, ref string) bool {
	if view == nil {
		return false
	}
	if ref == view.ID {
		return true
	}
	for _, tag := range view.RepoTags {
		if ref == tag {
			return true
		}
	}
	for _, repoDigest := range view.RepoDigests {
		if ref == repoDigest {
			return true
		}
	}
	return false
}

func imageReady(record runtimeapi.ImageRecord) bool {
	if isLegacyInternalBuildImage(record.Name) {
		return false
	}
	kind := normalizeImageKind(record)
	switch kind {
	case "sandbox-base", "sandbox-snapshot":
		return true
	case "rootfs", "sandbox", "mem-snapshot":
		return false
	default:
		return false
	}
}

func isLegacyInternalBuildImage(name string) bool {
	name = strings.TrimSpace(name)
	return strings.HasPrefix(name, "conch-erofs-rootfs:convert-") ||
		strings.HasPrefix(name, "conch-kernel:convert-")
}

func normalizeImageKind(record runtimeapi.ImageRecord) string {
	if kind := strings.TrimSpace(record.Kind); kind != "" {
		return kind
	}
	for _, key := range []string{"io.conch.kind", "conch.io/kind", "kind"} {
		if kind := strings.TrimSpace(record.Labels[key]); kind != "" {
			return kind
		}
	}
	return ""
}

func structureComponents(kind string) []string {
	switch kind {
	case "sandbox-base":
		return []string{"rootfs", "sandbox"}
	case "sandbox-snapshot":
		return []string{"rootfs", "sandbox", "mem-snapshot"}
	case "rootfs", "sandbox", "mem-snapshot":
		return []string{kind}
	default:
		return nil
	}
}

func appendIfMissing(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func cloneMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = v
	}
	return out
}
