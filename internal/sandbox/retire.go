package sandbox

import (
	"context"
	"strings"

	"github.com/containerd/errdefs"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/pkg/ulog"
)

// retireMemRecord best-effort deletes a checkpoint head's mem component record so the mem layer can be collected once the head loses its designed roots; the record is a pure cache recreated on demand, and source-template heads stay pinned by their Template record.
func (m *Manager) retireMemRecord(ctx context.Context, head, sourceTemplateID string) {
	head = strings.TrimSpace(head)
	if head == "" || head == sourceTemplateID {
		return
	}
	info, err := conchimage.InspectBootIndex(ctx, m.client, head)
	if err != nil {
		ulog.GetLogger().Warn("retire mem component record: inspect head", ulog.F("head", head), ulog.F("error", err))
		return
	}
	if info.MemDescriptor.Digest == "" {
		return
	}
	name := conchimage.ComponentImageName(conchimage.KindMemSnapshot, info.MemDescriptor.Digest)
	if err := m.client.ImageService().Delete(containerdclient.NewNamespaceContext(ctx), name); err != nil && !errdefs.IsNotFound(err) {
		ulog.GetLogger().Warn("retire mem component record", ulog.F("name", name), ulog.F("error", err))
	}
}
