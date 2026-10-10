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
	"github.com/openeuler/Conch/internal/envd"
	"github.com/openeuler/Conch/internal/id"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/netstack"
	slotstate "github.com/openeuler/Conch/internal/netstack/slot"
	"github.com/openeuler/Conch/internal/runtimeapi"
	conchtemplate "github.com/openeuler/Conch/internal/template"
	"github.com/openeuler/Conch/internal/webhook"
	"github.com/openeuler/Conch/pkg/requestid"
	"github.com/openeuler/Conch/pkg/ulog"
)

// Create owns the entire transition from CREATING to a persisted, ready
// sandbox under one lock hold. TemplateID is an immutable Boot Index digest;
// TemplateName is only provenance and is never resolved by Manager. E2B
// requests additionally reserve capacity, hold a route slot for the whole
// bootstrap and publish it only after envd is up.
func (m *Manager) Create(parent context.Context, req CreateRequest) (_ runtimeapi.SandboxCreateResult, err error) {
	if m == nil || m.store == nil || m.client == nil {
		return runtimeapi.SandboxCreateResult{}, fmt.Errorf("sandbox manager is not configured")
	}
	if err = m.validateCreateRequest(parent, &req); err != nil {
		return runtimeapi.SandboxCreateResult{}, err
	}
	if req.E2B {
		if m.Routes == nil || m.Capacity == nil || m.Envd == nil {
			return runtimeapi.SandboxCreateResult{}, fmt.Errorf("E2B runtime dependencies are not configured")
		}
		if req.Timeout <= 0 {
			return runtimeapi.SandboxCreateResult{}, ErrInvalidArgument.Wrap(fmt.Errorf("timeout must be positive for E2B sandboxes"))
		}
	}
	ctx, cancel := context.WithTimeout(parent, m.requestTimeout)
	defer cancel()
	unlock := m.lifecycleLocks.lock(req.SandboxID)
	defer unlock()
	if _, exists := m.sandboxes.Load(req.SandboxID); exists {
		return runtimeapi.SandboxCreateResult{}, ErrAlreadyExists.New()
	}
	var routeCtx context.Context
	if req.E2B {
		// The capacity reservation follows the record: every failure path
		// that finalizes the record releases it, an unconfirmed cleanup
		// keeps it for an explicit delete or startup recovery.
		if err := m.Capacity.Reserve(req.SandboxID, req.VCPUNum, req.RAMMB); err != nil {
			return runtimeapi.SandboxCreateResult{}, err
		}
		routeCtx = m.Routes.Insert(req.SandboxID)
		if routeCtx == nil {
			m.Capacity.Release(req.SandboxID)
			return runtimeapi.SandboxCreateResult{}, ErrFailedPrecondition.Wrap(fmt.Errorf("sandbox proxy registry is closed"))
		}
		// The client-visible bootstrap budget: the vsock wait and the envd
		// handshake share it, and late proxy requests wait the same span.
		var bootstrapCancel context.CancelFunc
		ctx, bootstrapCancel = context.WithTimeout(ctx, bootstrapTimeout)
		defer bootstrapCancel()
	}
	return m.createLocked(ctx, req, routeCtx, nil)
}

// createLocked runs the create kernel under the sandbox's lifecycle lock. A
// nil resumeRec starts a fresh sandbox whose head is the template; a resume
// passes the existing record and keeps its checkpoint head as the boot
// source. The lock is held for the whole transition, bootstrap included.
func (m *Manager) createLocked(ctx context.Context, req CreateRequest, routeCtx context.Context, resumeRec *Record) (_ runtimeapi.SandboxCreateResult, err error) {
	entry := &sandboxEntry{state: StateCreating, sourceRequestID: requestid.FromContext(ctx)}
	m.sandboxes.Store(req.SandboxID, entry)
	var rec Record
	ownsRecord := false
	leaseCreated := false
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), createCleanupTimeout)
		defer cleanupCancel()
		if err == nil {
			if leaseCreated {
				if releaseErr := m.releaseCreateLease(cleanupCtx, req.SandboxID); releaseErr != nil {
					// READY already owns the snapshots. A failed lease deletion is also
					// retried on sandbox deletion/recovery using the deterministic lease ID.
					ulog.WithContext(cleanupCtx).Warn("failed to release sandbox create lease", ulog.F("sandbox_id", req.SandboxID), ulog.F("error", releaseErr))
				}
			}
			return
		}
		entry.state = StateUnknown
		cleanupErr := m.cleanupSandbox(cleanupCtx, req.SandboxID, entry)
		if cleanupErr == nil && leaseCreated {
			cleanupErr = m.releaseCreateLease(cleanupCtx, req.SandboxID)
			if cleanupErr == nil {
				leaseCreated = false
			}
		}
		if cleanupErr == nil && ownsRecord {
			// A failed in-place resume keeps its checkpoint lineage, a fresh create has none.
			err = combineOperationErrors(err, m.finalizeReleased(cleanupCtx, rec, finalizationOptions{}))
			return
		}
		if cleanupErr == nil {
			// The CREATING write never succeeded: nothing of this sandbox is
			// persisted, so there is no record to finalize.
			m.sandboxes.Delete(req.SandboxID)
			if req.E2B {
				m.Capacity.Release(req.SandboxID)
			}
			return
		}
		// An unconfirmed cleanup retains the record as UNKNOWN, still owning
		// every resource an explicit delete or startup recovery must return,
		// including the capacity reservation and any remaining lease.
		entry.state = StateUnknown
		if !ownsRecord {
			err = combineOperationErrors(err, cleanupErr)
			m.sandboxes.Delete(req.SandboxID)
			if req.E2B {
				m.Capacity.Release(req.SandboxID)
			}
			return
		}
		rec.State = StateUnknown
		rec.LastError = errors.Join(err, cleanupErr).Error()
		_, saveErr := m.store.Update(cleanupCtx, rec)
		err = combineOperationErrors(err, cleanupErr, saveErr)
	}()
	rec, err = m.writeCreatingRecord(ctx, req, resumeRec)
	if err != nil {
		return runtimeapi.SandboxCreateResult{}, err
	}
	ownsRecord = true
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
	if m.memOvercommitRatio > 0 {
		if err = m.reserveRAM(vmSpec.MemorySizeMB); err != nil {
			ulog.WithContext(ctx).Warn("sandbox create rejected by memory admission", ulog.F("sandbox_id", req.SandboxID), ulog.F("error", err))
			return runtimeapi.SandboxCreateResult{}, err
		}
		entry.ramMB = vmSpec.MemorySizeMB
	}
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
	envdVersion := ""
	if req.E2B {
		envdVersion, err = m.bootstrapEnvd(ctx, routeCtx, rec.IP, req.Env)
		if err != nil {
			return runtimeapi.SandboxCreateResult{}, err
		}
		if !bootstrapAlive(sbx.process.Done()) {
			return runtimeapi.SandboxCreateResult{}, ErrFailedPrecondition.Wrap(fmt.Errorf(
				"sandbox %s exited during bootstrap", req.SandboxID,
			))
		}
		if err := m.Routes.Publish(req.SandboxID, rec.IP, req.MaskRequestHost); err != nil {
			return runtimeapi.SandboxCreateResult{}, err
		}
		rec.EnvdVersion = envdVersion
		rec.ExpiresAt = time.Now().Add(req.Timeout).UnixNano()
	}
	// The READY commit starts the run; StartedAt marks its beginning for both
	// the fresh and the resume branch.
	rec.State = StateReady
	rec.StartedAt = time.Now().UnixNano()
	_, err = m.store.Update(ctx, rec)
	if err != nil {
		return runtimeapi.SandboxCreateResult{}, fmt.Errorf("persist ready sandbox: %w", err)
	}
	entry.state = StateReady
	m.trackSandbox(req.SandboxID, entry, volumeExit)
	m.publishLifecycleEvent(ctx, webhook.EventSandboxCreated, rec, "")
	return runtimeapi.SandboxCreateResult{
		SandboxID: req.SandboxID, IP: rec.IP, AgentToken: req.AgentToken,
		TemplateName: req.TemplateName, TemplateID: req.TemplateID,
		VCPUNum: rec.VCPUNum, RamMB: rec.RamMB, CreatedAt: rec.CreatedAt,
		EnvdVersion: envdVersion,
	}, nil
}

// writeCreatingRecord persists the CREATING state that reserves the sandbox
// ID. A fresh create inserts the record; an in-place resume reuses the
// existing record and its checkpoint head.
func (m *Manager) writeCreatingRecord(ctx context.Context, req CreateRequest, resumeRec *Record) (Record, error) {
	if resumeRec == nil {
		rec, err := m.store.Create(ctx, Record{
			ID: req.SandboxID, State: StateCreating, CreatedAt: time.Now().UnixNano(),
			SourceTemplateName: req.TemplateName, SourceTemplateID: req.TemplateID,
			CheckpointHeadTemplateID: req.TemplateID,
			VCPUNum:                  req.VCPUNum, RamMB: req.RAMMB, Network: req.Network,
			E2B: req.E2B, TimeoutAction: req.TimeoutAction, AutoResume: req.AutoResume, MaskRequestHost: req.MaskRequestHost,
			Metadata: cloneStringMap(req.Metadata),
		})
		if err != nil {
			return Record{}, fmt.Errorf("persist creating sandbox: %w", err)
		}
		return rec, nil
	}
	rec := *resumeRec
	rec.State = StateCreating
	rec.LastError = ""
	rec.IP, rec.VMMPID = "", 0
	rec.RuntimeSnapshots = nil
	updated, err := m.store.Update(ctx, rec)
	if err != nil {
		return Record{}, fmt.Errorf("persist resuming sandbox: %w", err)
	}
	return updated, nil
}

// bootstrapEnvd waits for envd health and initialization on the route slot's
// context: a slot replacement or release stops the handshake, while the
// request context stays the parent so a client disconnect stops it too.
func (m *Manager) bootstrapEnvd(ctx context.Context, routeCtx context.Context, ip string, env map[string]string) (string, error) {
	var envdCtx context.Context
	var cancel context.CancelFunc
	if deadline, ok := ctx.Deadline(); ok {
		envdCtx, cancel = context.WithDeadline(routeCtx, deadline)
	} else {
		envdCtx, cancel = context.WithCancel(routeCtx)
	}
	stop := context.AfterFunc(ctx, cancel)
	defer func() { stop(); cancel() }()
	if err := m.Envd.WaitReady(envdCtx, ip); err != nil {
		return "", fmt.Errorf("wait for envd: %w", err)
	}
	version, err := m.Envd.Init(envdCtx, ip, envd.InitOptions{
		EnvVars: cloneStringMap(env), DefaultUser: "user", DefaultWorkdir: "/home/user",
	})
	if err != nil {
		return "", fmt.Errorf("initialize envd: %w", err)
	}
	return version, nil
}

// bootstrapAlive is the liveness probe between envd init and route publish:
// the exit channel closes before any watcher can take the lock, so it is the
// only death signal visible inside the create critical section.
func bootstrapAlive(done <-chan struct{}) bool {
	select {
	case <-done:
		return false
	default:
		return true
	}
}

func (m *Manager) reserveRAM(requestedMB int64) error {
	limitMB := float64(m.memLimitMB) * m.memOvercommitRatio
	for {
		usedMB := m.usedRAM.Load()
		if float64(usedMB)+float64(requestedMB) > limitMB {
			err := fmt.Errorf(
				"sandbox memory limit exceeded: requested=%dMiB used=%dMiB limit=%dMiB overcommit_ratio=%g effective_limit=%gMiB",
				requestedMB, usedMB, m.memLimitMB, m.memOvercommitRatio, limitMB,
			)
			return ErrResourceExhausted.WrapMessage(err, "sandbox memory limit exceeded")
		}
		if m.usedRAM.CompareAndSwap(usedMB, usedMB+requestedMB) {
			return nil
		}
	}
}

func (m *Manager) releaseRAM(entry *sandboxEntry) {
	if entry == nil || entry.ramMB == 0 {
		return
	}
	ramMB := entry.ramMB
	entry.ramMB = 0
	m.usedRAM.Add(-ramMB)
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
		err = validateSandboxID(req.SandboxID)
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
		return ErrInvalidArgument.Wrap(fmt.Errorf("sandbox CPU or memory request exceeds limits"))
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

// validateSandboxID reserves names used by runtime memory and view snapshots.
func validateSandboxID(sandboxID string) error {
	if err := id.Validate(sandboxID); err != nil {
		return err
	}
	if strings.HasSuffix(sandboxID, "-mem") {
		return fmt.Errorf("sandbox id suffix %q is reserved for internal snapshots", "-mem")
	}
	for _, prefix := range []string{"view-rootfs-", "view-mem-", "view-vm-"} {
		if strings.HasPrefix(sandboxID, prefix) {
			return fmt.Errorf("sandbox id prefix %q is reserved for internal snapshots", prefix)
		}
	}
	return nil
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
