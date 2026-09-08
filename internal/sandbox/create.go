package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	agentprotocol "github.com/openeuler/Conch/internal/agent/protocol"
	"github.com/openeuler/Conch/internal/apperror"
	"github.com/openeuler/Conch/internal/id"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/netstack"
	slotstate "github.com/openeuler/Conch/internal/netstack/slot"
	"github.com/openeuler/Conch/internal/runtimeapi"
	conchtemplate "github.com/openeuler/Conch/internal/template"
	"github.com/openeuler/Conch/internal/webhook"
	"github.com/openeuler/Conch/pkg/ulog"
)

// Create owns the entire transition from CREATING to a persisted, ready
// sandbox. TemplateID is an immutable Boot Index digest; TemplateName is only
// provenance and is never resolved by Manager.
func (m *Manager) Create(parent context.Context, req CreateRequest) (_ runtimeapi.SandboxCreateResult, err error) {
	if m == nil || m.store == nil || m.client == nil {
		return runtimeapi.SandboxCreateResult{}, fmt.Errorf("sandbox manager is not configured")
	}
	if err = m.validateCreateRequest(parent, &req); err != nil {
		return runtimeapi.SandboxCreateResult{}, err
	}
	ctx, cancel := context.WithTimeout(parent, m.requestTimeout)
	defer cancel()
	unlock := m.lifecycleLocks.lock(req.SandboxID)
	defer unlock()
	if _, exists := m.sandboxes.Load(req.SandboxID); exists {
		return runtimeapi.SandboxCreateResult{}, ErrAlreadyExists.New()
	}
	// Store.Create is the persistent ID reservation; no preceding Get is needed.
	rec, err := m.store.Create(ctx, Record{
		ID: req.SandboxID, State: StateCreating, CreatedAt: time.Now().UnixNano(),
		SourceTemplateName: req.TemplateName, SourceTemplateID: req.TemplateID,
		CheckpointHeadTemplateID: req.TemplateID,
		VCPUNum:                  req.VCPUNum, RamMB: req.RAMMB, Network: req.Network,
	})
	if err != nil {
		return runtimeapi.SandboxCreateResult{}, fmt.Errorf("persist creating sandbox: %w", err)
	}
	entry := &sandboxEntry{state: StateCreating}
	m.sandboxes.Store(req.SandboxID, entry)
	leaseCreated := false
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), createCleanupTimeout)
		defer cleanupCancel()
		if err != nil {
			cleanupErr := m.cleanupSandbox(cleanupCtx, req.SandboxID, entry)
			if cleanupErr == nil && leaseCreated {
				cleanupErr = m.releaseCreateLease(cleanupCtx, req.SandboxID)
				if cleanupErr == nil {
					leaseCreated = false
				}
			}
			if cleanupErr != nil {
				// Keep the CREATING record and any remaining lease for recovery, including a
				// possibly running VMM whose creation did not finish.
				entry.state = StateUnknown
				rec.State = StateCreating
				rec.LastError = errors.Join(err, cleanupErr).Error()
				_, saveErr := m.store.Update(cleanupCtx, rec)
				err = combineOperationErrors(err, cleanupErr, saveErr)
				return
			}
			m.sandboxes.Delete(req.SandboxID)
			if deleteErr := m.store.Delete(cleanupCtx, req.SandboxID); deleteErr != nil {
				err = combineOperationErrors(err, deleteErr)
				return
			}
		}
		if leaseCreated {
			if releaseErr := m.releaseCreateLease(cleanupCtx, req.SandboxID); releaseErr != nil {
				// READY already owns the snapshots. A failed lease deletion is also
				// retried on sandbox deletion/recovery using the deterministic lease ID.
				ulog.GetLogger().Warn("failed to release sandbox create lease", ulog.F("sandbox_id", req.SandboxID), ulog.F("error", releaseErr))
			}
		}
	}()
	lease, err := m.client.LeasesService().Create(containerdclient.NewNamespaceContext(ctx), leases.WithID(createLeaseID(req.SandboxID)))
	if err != nil {
		return runtimeapi.SandboxCreateResult{}, fmt.Errorf("create sandbox lease: %w", err)
	}
	leaseCreated = true
	ctx = leases.WithLease(ctx, lease.ID)
	runtimeIDs, err := m.allocateCreateRuntimeIDs(req)
	if err != nil {
		return runtimeapi.SandboxCreateResult{}, err
	}
	boot, err := m.prepareSandboxBoot(ctx, req)
	if err != nil {
		return runtimeapi.SandboxCreateResult{}, translateBootError(err)
	}
	rec.RuntimeSnapshots = append([]SnapshotRef(nil), boot.RuntimeSnapshots...)
	rec.RamMB = boot.Spec.MemorySizeMB
	vmSpec := boot.Spec
	devices, err := m.prepareVolumes(req, boot.Resume)
	entry.volumes = devices
	if err != nil {
		return runtimeapi.SandboxCreateResult{}, err
	}
	var volumeExit <-chan struct{}
	if len(devices) > 0 {
		volumeExit = devices[0].Exited
	}
	vmSpec.VirtioFS = volumeDevicesToDriver(devices)
	sbx, err := m.launch(ctx, req, vmSpec, runtimeIDs, boot.Resume)
	if sbx != nil {
		entry.sbx = sbx
		if sbx.process != nil {
			rec.VMMPID = sbx.process.Pid()
		}
	}
	if err != nil {
		return runtimeapi.SandboxCreateResult{}, translateStartError(err)
	}
	rec.IP = sbx.slot.CNIIP()
	rec.State = StateReady
	if _, err = m.store.Update(ctx, rec); err != nil {
		return runtimeapi.SandboxCreateResult{}, fmt.Errorf("persist ready sandbox: %w", err)
	}
	entry.state = StateReady
	m.trackSandbox(req.SandboxID, entry, volumeExit)
	m.publishLifecycleEvent(webhook.EventSandboxCreated, rec, "")
	return runtimeapi.SandboxCreateResult{
		SandboxID: req.SandboxID, IP: rec.IP, AgentToken: req.AgentToken,
		TemplateName: req.TemplateName, TemplateID: req.TemplateID,
		VCPUNum: rec.VCPUNum, RamMB: rec.RamMB, CreatedAt: rec.CreatedAt,
	}, nil
}

func (m *Manager) validateCreateRequest(ctx context.Context, req *CreateRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	req.SandboxID = strings.TrimSpace(req.SandboxID)
	var err error
	if req.SandboxID == "" {
		req.SandboxID, err = id.New()
	} else {
		err = id.Validate(req.SandboxID)
	}
	if err != nil {
		return ErrInvalidArgument.Wrap(err)
	}
	if _, err := digest.Parse(req.TemplateID); err != nil {
		return ErrInvalidArgument.Wrap(fmt.Errorf("invalid template_id: %w", err))
	}
	if _, ok := m.vmmBinaries[req.VMMName]; !ok {
		return ErrInvalidArgument.Wrap(fmt.Errorf("vmm %q is not configured", req.VMMName))
	}
	if err := validateVCPUNum(req.VCPUNum, req.VCPUMax); err != nil {
		return ErrInvalidArgument.Wrap(err)
	}
	if req.RAMMB <= 0 {
		return ErrInvalidArgument.Wrap(fmt.Errorf("ram_mb must be positive"))
	}
	if req.VCPUNum > runtimeapi.SandboxMaxVCPU || req.VCPUMax > runtimeapi.SandboxMaxVCPU || req.RAMMB > runtimeapi.SandboxMaxRAMMB {
		return ErrResourceExhausted.Wrap(fmt.Errorf("sandbox CPU or memory request exceeds limits"))
	}
	if err := agentprotocol.ValidateEnvironment(req.Env); err != nil {
		return ErrInvalidEnvironment.Wrap(err)
	}
	if err := netstack.ValidateSandboxNetworkInputConfig(ctx, req.Network); err != nil {
		return err
	}
	req.Env = cloneStringMap(req.Env)
	req.AgentToken, err = GenerateAgentToken()
	return err
}

func GenerateAgentToken() (string, error) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return "", fmt.Errorf("generate agent token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(token), nil
}

func createLeaseID(sandboxID string) string { return "sandbox-create-" + sandboxID }

func (m *Manager) releaseCreateLease(ctx context.Context, sandboxID string) error {
	err := m.client.LeasesService().Delete(containerdclient.NewNamespaceContext(ctx), leases.Lease{ID: createLeaseID(sandboxID)})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func translateBootError(err error) error {
	switch {
	case errors.Is(err, conchimage.ErrNotFound):
		return conchtemplate.ErrNotFound.Wrap(err)
	case errors.Is(err, conchimage.ErrInvalidArgument), errors.Is(err, conchimage.ErrInvalidContent):
		return conchtemplate.ErrInvalidArtifact.Wrap(err)
	default:
		return err
	}
}

func translateStartError(err error) error {
	switch {
	case errors.Is(err, agentprotocol.ErrInvalidEnvironment):
		return ErrInvalidEnvironment.Wrap(err)
	case errors.Is(err, agentprotocol.ErrPayloadTooLarge):
		return ErrInitializationTooLarge.Wrap(err)
	case errors.Is(err, slotstate.ErrEmpty), errors.Is(err, slotstate.ErrCapacity):
		return ErrResourceExhausted.Wrap(err)
	default:
		return fmt.Errorf("start sandbox: %w", err)
	}
}

// Preserve the primary operation's application classification when cleanup
// also fails. Secondary errors must not turn an internal error into a 4xx.
func combineOperationErrors(primary error, secondary ...error) error {
	if primary == nil {
		return errors.Join(secondary...)
	}
	additional := errors.Join(secondary...)
	if additional == nil {
		return primary
	}
	var appErr *apperror.Error
	if errors.As(primary, &appErr) {
		return appErr.WrapMessage(errors.Join(primary, additional), appErr.PublicMessage())
	}
	return fmt.Errorf("%w; additional operation failures: %v", primary, additional)
}
