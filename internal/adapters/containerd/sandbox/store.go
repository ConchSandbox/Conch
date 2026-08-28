package containerdsandbox

import (
	"context"
	"fmt"
	"sort"
	"strings"

	cdsandbox "github.com/containerd/containerd/v2/core/sandbox"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/runtimeapi"
	conchsandbox "github.com/openeuler/Conch/internal/sandbox"
)

const (
	sandboxerName         = "conch"
	extensionName         = "io.conch.sandbox.metadata.v1"
	gcSnapshotLabelPrefix = "containerd.io/gc.ref.snapshot."
)

type metadataV1 struct {
	VMMPID                   int                              `json:"vmm_pid,omitempty"`
	State                    string                           `json:"state"`
	SourceTemplateName       string                           `json:"source_template_name,omitempty"`
	SourceTemplateID         string                           `json:"source_template_id,omitempty"`
	CheckpointHeadTemplateID string                           `json:"checkpoint_head_template_id,omitempty"`
	IP                       string                           `json:"ip,omitempty"`
	VCPUNum                  int64                            `json:"vcpu_num,omitempty"`
	RamMB                    int64                            `json:"ram_mb,omitempty"`
	Network                  *runtimeapi.SandboxNetworkConfig `json:"network,omitempty"`
	LastError                string                           `json:"last_error,omitempty"`
}

func init() {
	typeurl.Register(&metadataV1{}, extensionName)
}

type Store struct {
	store cdsandbox.Store
}

func NewStore(store cdsandbox.Store) *Store {
	return &Store{store: store}
}

func (s *Store) Create(ctx context.Context, record conchsandbox.Record) (conchsandbox.Record, error) {
	if err := s.validate(record); err != nil {
		return conchsandbox.Record{}, err
	}
	extension, err := typeurl.MarshalAny(metadataFromRecord(record))
	if err != nil {
		return conchsandbox.Record{}, fmt.Errorf("marshal Sandbox metadata: %w", err)
	}
	labels, err := encodeSnapshotRefs(record.RuntimeSnapshots)
	if err != nil {
		return conchsandbox.Record{}, err
	}
	native, err := s.store.Create(containerdclient.NewNamespaceContext(ctx), cdsandbox.Sandbox{
		ID:         record.ID,
		Labels:     labels,
		Sandboxer:  sandboxerName,
		Extensions: map[string]typeurl.Any{extensionName: extension},
	})
	if err != nil {
		return conchsandbox.Record{}, translateError("create Sandbox record", err)
	}
	return recordFromNative(native)
}

func (s *Store) Update(ctx context.Context, record conchsandbox.Record) (conchsandbox.Record, error) {
	if err := s.validate(record); err != nil {
		return conchsandbox.Record{}, err
	}
	nsctx := containerdclient.NewNamespaceContext(ctx)
	native, err := s.store.Get(nsctx, record.ID)
	if err != nil {
		return conchsandbox.Record{}, translateError("get Sandbox record", err)
	}
	if native.Sandboxer != sandboxerName {
		return conchsandbox.Record{}, conchsandbox.ErrNotFound.Wrap(fmt.Errorf("Sandbox %s is not owned by Conch", record.ID))
	}
	extension, err := typeurl.MarshalAny(metadataFromRecord(record))
	if err != nil {
		return conchsandbox.Record{}, fmt.Errorf("marshal Sandbox metadata: %w", err)
	}
	labels, err := replaceSnapshotRefs(native.Labels, record.RuntimeSnapshots)
	if err != nil {
		return conchsandbox.Record{}, err
	}
	if native.Extensions == nil {
		native.Extensions = make(map[string]typeurl.Any)
	}
	native.Labels = labels
	native.Extensions[extensionName] = extension
	native, err = s.store.Update(nsctx, native, "labels", "extensions."+extensionName)
	if err != nil {
		return conchsandbox.Record{}, translateError("update Sandbox record", err)
	}
	return recordFromNative(native)
}

func (s *Store) Get(ctx context.Context, id string) (conchsandbox.Record, error) {
	if strings.TrimSpace(id) == "" {
		return conchsandbox.Record{}, conchsandbox.ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	native, err := s.store.Get(containerdclient.NewNamespaceContext(ctx), id)
	if err != nil {
		return conchsandbox.Record{}, translateError("get Sandbox record", err)
	}
	if native.Sandboxer != sandboxerName {
		return conchsandbox.Record{}, conchsandbox.ErrNotFound.Wrap(fmt.Errorf("Sandbox %s is not owned by Conch", id))
	}
	return recordFromNative(native)
}

func (s *Store) List(ctx context.Context, filter conchsandbox.Filter) ([]conchsandbox.Record, error) {
	if filter.State != "" && !validState(filter.State) {
		return nil, conchsandbox.ErrInvalidArgument.Wrap(fmt.Errorf("unknown sandbox state %q", filter.State))
	}
	nativeRecords, err := s.store.List(containerdclient.NewNamespaceContext(ctx))
	if err != nil {
		return nil, translateError("list Sandbox records", err)
	}
	records := make([]conchsandbox.Record, 0, len(nativeRecords))
	for _, native := range nativeRecords {
		if native.Sandboxer != sandboxerName {
			continue
		}
		record, err := recordFromNative(native)
		if err != nil {
			return nil, err
		}
		if filter.State != "" && record.State != filter.State {
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return records, nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return conchsandbox.ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	nsctx := containerdclient.NewNamespaceContext(ctx)
	native, err := s.store.Get(nsctx, id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return translateError("get Sandbox record", err)
	}
	if native.Sandboxer != sandboxerName {
		return nil
	}
	if err := s.store.Delete(nsctx, id); err != nil && !errdefs.IsNotFound(err) {
		return translateError("delete Sandbox record", err)
	}
	return nil
}

func (s *Store) validate(record conchsandbox.Record) error {
	if s == nil || s.store == nil {
		return fmt.Errorf("sandbox store is not configured")
	}
	if strings.TrimSpace(record.ID) == "" {
		return conchsandbox.ErrInvalidArgument.Wrap(fmt.Errorf("sandbox id is required"))
	}
	if !validState(record.State) {
		return conchsandbox.ErrInvalidArgument.Wrap(fmt.Errorf("unknown sandbox state %q", record.State))
	}
	if record.State == conchsandbox.StateCreating {
		if strings.TrimSpace(record.SourceTemplateID) == "" {
			return conchsandbox.ErrInvalidArgument.Wrap(fmt.Errorf("creating sandbox source Template ID is required"))
		}
	} else if strings.TrimSpace(record.CheckpointHeadTemplateID) == "" {
		return conchsandbox.ErrInvalidArgument.Wrap(fmt.Errorf("sandbox checkpoint head Template ID is required"))
	}
	return nil
}

func validState(state conchsandbox.State) bool {
	switch state {
	case conchsandbox.StateCreating, conchsandbox.StateReady, conchsandbox.StateSuspended, conchsandbox.StateUnknown:
		return true
	default:
		return false
	}
}

func metadataFromRecord(record conchsandbox.Record) *metadataV1 {
	return &metadataV1{
		VMMPID: record.VMMPID, State: string(record.State), SourceTemplateName: record.SourceTemplateName,
		SourceTemplateID:         record.SourceTemplateID,
		CheckpointHeadTemplateID: record.CheckpointHeadTemplateID, IP: record.IP, VCPUNum: record.VCPUNum,
		RamMB: record.RamMB, Network: record.Network, LastError: record.LastError,
	}
}

func recordFromNative(native cdsandbox.Sandbox) (conchsandbox.Record, error) {
	extension, ok := native.Extensions[extensionName]
	if !ok {
		return conchsandbox.Record{}, conchsandbox.ErrFailedPrecondition.Wrap(fmt.Errorf("Sandbox %s has no Conch metadata", native.ID))
	}
	var metadata metadataV1
	if err := typeurl.UnmarshalTo(extension, &metadata); err != nil {
		return conchsandbox.Record{}, conchsandbox.ErrFailedPrecondition.Wrap(fmt.Errorf("decode Sandbox %s metadata: %w", native.ID, err))
	}
	state := conchsandbox.State(metadata.State)
	if !validState(state) {
		return conchsandbox.Record{}, conchsandbox.ErrFailedPrecondition.Wrap(fmt.Errorf("Sandbox %s has unknown state %q", native.ID, metadata.State))
	}
	refs, err := decodeSnapshotRefs(native.Labels)
	if err != nil {
		return conchsandbox.Record{}, conchsandbox.ErrFailedPrecondition.Wrap(fmt.Errorf("decode Sandbox %s snapshot references: %w", native.ID, err))
	}
	return conchsandbox.Record{
		ID: native.ID, VMMPID: metadata.VMMPID, State: state, CreatedAt: native.CreatedAt.UnixNano(),
		SourceTemplateName: metadata.SourceTemplateName, SourceTemplateID: metadata.SourceTemplateID,
		CheckpointHeadTemplateID: metadata.CheckpointHeadTemplateID,
		IP:                       metadata.IP, VCPUNum: metadata.VCPUNum, RamMB: metadata.RamMB, Network: metadata.Network,
		LastError: metadata.LastError, RuntimeSnapshots: refs,
	}, nil
}

func replaceSnapshotRefs(existing map[string]string, refs []conchsandbox.SnapshotRef) (map[string]string, error) {
	labels := make(map[string]string, len(existing)+len(refs))
	for key, value := range existing {
		if !strings.HasPrefix(key, gcSnapshotLabelPrefix) {
			labels[key] = value
		}
	}
	encoded, err := encodeSnapshotRefs(refs)
	if err != nil {
		return nil, err
	}
	for key, value := range encoded {
		labels[key] = value
	}
	return labels, nil
}

func encodeSnapshotRefs(refs []conchsandbox.SnapshotRef) (map[string]string, error) {
	labels := make(map[string]string, len(refs))
	for _, ref := range refs {
		snapshotter, role, key := strings.TrimSpace(ref.Snapshotter), strings.TrimSpace(ref.Role), strings.TrimSpace(ref.Key)
		if snapshotter == "" || role == "" || key == "" || strings.Contains(snapshotter, "/") || strings.Contains(role, "/") {
			return nil, conchsandbox.ErrInvalidArgument.Wrap(fmt.Errorf("invalid runtime snapshot reference %#v", ref))
		}
		label := gcSnapshotLabelPrefix + snapshotter + "/" + role
		if _, exists := labels[label]; exists {
			return nil, conchsandbox.ErrInvalidArgument.Wrap(fmt.Errorf("duplicate runtime snapshot role %s/%s", snapshotter, role))
		}
		labels[label] = key
	}
	return labels, nil
}

func decodeSnapshotRefs(labels map[string]string) ([]conchsandbox.SnapshotRef, error) {
	refs := make([]conchsandbox.SnapshotRef, 0)
	for label, key := range labels {
		if !strings.HasPrefix(label, gcSnapshotLabelPrefix) {
			continue
		}
		snapshotter, role, ok := strings.Cut(strings.TrimPrefix(label, gcSnapshotLabelPrefix), "/")
		if !ok || snapshotter == "" || role == "" || key == "" {
			return nil, fmt.Errorf("invalid snapshot GC label %q=%q", label, key)
		}
		refs = append(refs, conchsandbox.SnapshotRef{Snapshotter: snapshotter, Role: role, Key: key})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Snapshotter != refs[j].Snapshotter {
			return refs[i].Snapshotter < refs[j].Snapshotter
		}
		return refs[i].Role < refs[j].Role
	})
	return refs, nil
}

func translateError(action string, err error) error {
	switch {
	case errdefs.IsNotFound(err):
		return conchsandbox.ErrNotFound.Wrap(err)
	case errdefs.IsAlreadyExists(err):
		return conchsandbox.ErrAlreadyExists.Wrap(err)
	case errdefs.IsInvalidArgument(err):
		return conchsandbox.ErrInvalidArgument.Wrap(err)
	default:
		return fmt.Errorf("%s: %w", action, err)
	}
}
