package conchruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/openeuler/Conch/internal/envd"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/internal/sandboxproxy"
	conchtemplate "github.com/openeuler/Conch/internal/template"
)

type memorySandboxStore struct {
	mu         sync.Mutex
	records    map[string]sandbox.Record
	operations []string
}

func newMemorySandboxStore() *memorySandboxStore {
	return &memorySandboxStore{records: make(map[string]sandbox.Record)}
}

func (s *memorySandboxStore) Create(_ context.Context, record sandbox.Record) (sandbox.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateMemorySandboxRecord(record); err != nil {
		return sandbox.Record{}, err
	}
	if _, ok := s.records[record.ID]; ok {
		return sandbox.Record{}, sandbox.ErrAlreadyExists.New()
	}
	if record.CreatedAt == 0 {
		record.CreatedAt = time.Now().UnixNano()
	}
	s.records[record.ID] = record
	s.operations = append(s.operations, "create:"+string(record.State))
	return record, nil
}

func (s *memorySandboxStore) Update(_ context.Context, record sandbox.Record) (sandbox.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateMemorySandboxRecord(record); err != nil {
		return sandbox.Record{}, err
	}
	if _, ok := s.records[record.ID]; !ok {
		return sandbox.Record{}, sandbox.ErrNotFound.New()
	}
	s.records[record.ID] = record
	s.operations = append(s.operations, "update:"+string(record.State))
	return record, nil
}

func validateMemorySandboxRecord(record sandbox.Record) error {
	if record.State == sandbox.StateCreating {
		if record.SourceTemplateID == "" {
			return sandbox.ErrInvalidArgument.New()
		}
		return nil
	}
	if record.CheckpointHeadTemplateID == "" {
		return sandbox.ErrInvalidArgument.New()
	}
	return nil
}

func (s *memorySandboxStore) Get(_ context.Context, id string) (sandbox.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[id]
	if !ok {
		return sandbox.Record{}, sandbox.ErrNotFound.New()
	}
	return record, nil
}

func (s *memorySandboxStore) List(_ context.Context, filter sandbox.Filter) ([]sandbox.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make([]sandbox.Record, 0, len(s.records))
	for _, record := range s.records {
		if filter.State == "" || record.State == filter.State {
			records = append(records, record)
		}
	}
	return records, nil
}

func (s *memorySandboxStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, id)
	s.operations = append(s.operations, "delete")
	return nil
}

func (s *memorySandboxStore) operationLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.operations...)
}

func (s *memorySandboxStore) Put(ctx context.Context, record sandbox.Record) error {
	if _, err := s.Get(ctx, record.ID); errors.Is(err, sandbox.ErrNotFound) {
		_, err = s.Create(ctx, record)
		return err
	}
	_, err := s.Update(ctx, record)
	return err
}

func TestManagerCreateErrorPreservesUnconfirmedBootstrapOwner(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfirmed", true: "released"}[confirmed], func(t *testing.T) {
			ctx := context.Background()
			store := newMemorySandboxStore()
			root := digest.FromString("bootstrap-template").String()
			cause := errors.New("conch-init readiness failed")
			// An unconfirmed Manager cleanup retains its record; a confirmed
			// one deletes it. Either way the E2B create fails at the runtime.
			ops := &fakeSandboxOps{store: store, createErr: cause, retainOnCreateFailure: !confirmed}
			svc := New(ops, nil)
			svc.Store = store
			setFakeTemplate(svc, testTemplateName, root, conchtemplate.BootModeCold)
			svc.Envd = envd.NewClient()
			svc.ProxyRoutes = sandboxproxy.NewRegistry()
			var err error
			svc.Capacity, err = NewCapacity(CapacityLimits{MaxSandboxes: 1, MaxCPUs: 2, MaxMemoryMB: 512})
			if err != nil {
				t.Fatal(err)
			}
			const sandboxID = "a541d234-baf9-4d67-a3ca-5696e49e39db"
			_, err = svc.CreateSandbox(ctx, SandboxCreateOptions{SandboxID: sandboxID, TemplateName: testTemplateName, VCPUNum: 2, VCPUMax: 2, RamMB: 512, E2B: true})
			if !errors.Is(err, cause) {
				t.Fatalf("bootstrap cause lost: %v", err)
			}
			if _, ok := svc.ProxyRoutes.CurrentGeneration(sandboxID); ok {
				t.Fatal("failed create left a proxy generation")
			}
			if confirmed {
				if _, err := store.Get(ctx, sandboxID); !errors.Is(err, sandbox.ErrNotFound) {
					t.Fatalf("released bootstrap retained record: %v", err)
				}
			} else {
				// The fake Manager keeps its record on a failed create without
				// confirmed cleanup; it still owns the capacity reservation.
				if err := svc.Capacity.reserve("next", 2, 512); !errors.Is(err, sandbox.ErrResourceExhausted) {
					t.Fatalf("unconfirmed VMM was refunded: %v", err)
				}
				record, err := store.Get(ctx, sandboxID)
				if err != nil {
					t.Fatalf("lost bootstrap owner: %v", err)
				}
				if record.State != sandbox.StateUnknown && record.State != sandbox.StateCreating {
					t.Fatalf("bootstrap owner state = %q", record.State)
				}
				// The production controller later confirms release on deletion.
				if err := svc.RemoveSandbox(ctx, sandboxID); err != nil {
					t.Fatal(err)
				}
			}
			if err := svc.Capacity.reserve("next", 2, 512); err != nil {
				t.Fatalf("confirmed bootstrap cleanup stranded capacity: %v", err)
			}
		})
	}
}

// localGuestEnvd stands in for a reachable guest: envd answers /health and
// /init, and conch-init's process API reports the envd version.
func localGuestEnvd(t *testing.T, ip string, initFailed *atomic.Bool, initialized *atomic.Bool) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/init", func(w http.ResponseWriter, r *http.Request) {
		if initFailed.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request["defaultUser"] != "user" || request["defaultWorkdir"] != "/home/user" ||
			request["envVars"].(map[string]any)["HELLO"] != "world" {
			t.Errorf("wrong envd init payload %#v", request)
		}
		initialized.Store(true)
		w.WriteHeader(http.StatusNoContent)
	})
	listener, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(envd.DefaultPort)))
	if err != nil {
		t.Skipf("guest IP %s unavailable: %v", ip, err)
	}
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
}

func TestCreateFailureReleasesCapacityAfterRollback(t *testing.T) {
	// The envd /init contract is exercised against a local guest; the envd
	// version handshake needs conch-init's authenticated process API and is
	// covered end-to-end by the E2B node API tests.
	ctx := context.Background()
	store := newMemorySandboxStore()
	templateID := digest.FromString("rollback-template").String()
	ops := &fakeSandboxOps{store: store, createResult: runtimeapi.SandboxCreateResult{
		SandboxID: "rollback", TemplateID: templateID, AgentToken: "agent-token",
	}}
	svc := New(ops, nil)
	svc.Store = store
	setFakeTemplate(svc, testTemplateName, templateID, conchtemplate.BootModeCold)
	svc.Envd = envd.NewClient()
	svc.ProxyRoutes = sandboxproxy.NewRegistry()
	var err error
	svc.Capacity, err = NewCapacity(CapacityLimits{MaxSandboxes: 1, MaxCPUs: 2, MaxMemoryMB: 512})
	if err != nil {
		t.Fatal(err)
	}

	initFailed := &atomic.Bool{}
	initialized := &atomic.Bool{}
	localGuestEnvd(t, "127.0.0.36", initFailed, initialized)
	// Route the fake guest through the create result IP.
	ops.createResult.IP = "127.0.0.36"

	const sandboxID = "b541d234-baf9-4d67-a3ca-5696e49e39db"
	// A failed envd /init must roll back VM, store record, route and capacity
	// so a subsequent create can consume the single available slot.
	initFailed.Store(true)
	if _, err := svc.CreateSandbox(ctx, SandboxCreateOptions{
		SandboxID: sandboxID, TemplateName: testTemplateName, VCPUNum: 2, VCPUMax: 2, RamMB: 512,
		E2B: true, Env: map[string]string{"HELLO": "world"}, Metadata: map[string]string{"purpose": "sdk"},
		Timeout: 60,
	}); err == nil {
		t.Fatal("failed init did not fail the create")
	}
	if ops.deleteCalls != 1 {
		t.Fatalf("rollback Delete calls = %d, want 1", ops.deleteCalls)
	}
	if _, ok := svc.ProxyRoutes.CurrentGeneration(sandboxID); ok {
		t.Fatal("failed init left a proxy generation")
	}
	if _, err := store.Get(ctx, sandboxID); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("failed init left persisted sandbox: %v", err)
	}
	if err := svc.Capacity.reserve("probe", 1, 64); err != nil {
		t.Fatalf("failed bootstrap stranded capacity: %v", err)
	}
	svc.Capacity.release("probe")

	// An unreachable guest (envd never becomes healthy) rolls back the same
	// way; the caller's deadline bounds the bootstrap wait.
	ops.createResult.IP = "192.0.2.99"
	deadlineCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := svc.CreateSandbox(deadlineCtx, SandboxCreateOptions{
		TemplateName: testTemplateName, VCPUNum: 2, VCPUMax: 2, RamMB: 512, E2B: true,
	}); err == nil {
		t.Fatal("unreachable envd did not fail the create")
	}
	if ops.deleteCalls != 2 {
		t.Fatalf("rollback Delete calls = %d, want 2", ops.deleteCalls)
	}
	if err := svc.Capacity.reserve("probe", 2, 512); err != nil {
		t.Fatalf("unreachable bootstrap stranded capacity: %v", err)
	}
	svc.Capacity.release("probe")
}

func TestMaintenanceObservationCannotDeleteReplacement(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "expiry", true: "cleanup"}[pending], func(t *testing.T) {
			ctx := context.Background()
			store := newMemorySandboxStore()
			templateID := digest.FromString("maintenance-identity-template").String()
			ops := &fakeSandboxOps{store: store}
			svc := New(ops, nil)
			svc.Store = store
			setFakeTemplate(svc, testTemplateName, templateID, conchtemplate.BootModeCold)
			var err error
			svc.Capacity, err = NewCapacity(CapacityLimits{MaxSandboxes: 1, MaxCPUs: 2, MaxMemoryMB: 512})
			if err != nil {
				t.Fatal(err)
			}
			opts := SandboxCreateOptions{SandboxID: "maintenance-id", TemplateName: testTemplateName, VCPUNum: 2, VCPUMax: 2, RamMB: 512}
			if _, err := svc.CreateSandbox(ctx, opts); err != nil {
				t.Fatal(err)
			}
			old, err := svc.GetSandbox(ctx, opts.SandboxID)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if pending {
				// Manager retained the record after an unconfirmed cleanup.
				old.State = sandbox.StateUnknown
			} else {
				old.E2B = true
				old.ExpiresAt = now.Add(-time.Second).UnixNano()
			}
			if _, err := store.Update(ctx, old); err != nil {
				t.Fatal(err)
			}
			// The worker retains this observation while explicit deletion and
			// native creation replace the runtime behind the same public ID.
			if err := svc.RemoveSandbox(ctx, old.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.CreateSandbox(ctx, opts); err != nil {
				t.Fatal(err)
			}
			replacement, err := svc.GetSandbox(ctx, opts.SandboxID)
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.Capacity.reserve(replacement.ID, replacement.VCPUNum, replacement.RamMB); err != nil {
				t.Fatal(err)
			}
			if err := svc.ReconcileSandbox(ctx, old.ID, old.RuntimeID, now); err != nil {
				t.Fatal(err)
			}
			after, err := svc.GetSandbox(ctx, replacement.ID)
			if err != nil || !reflect.DeepEqual(after, replacement) {
				t.Fatalf("stale maintenance deleted/changed replacement: %+v %v", after, err)
			}
			if err := svc.Capacity.reserve("extra", 2, 512); !errors.Is(err, sandbox.ErrResourceExhausted) {
				t.Fatalf("stale maintenance refunded capacity: %v", err)
			}
			// Even a matching identity must still pass the current eligibility
			// check; an observation is not authorization to delete a live VM.
			replacement.E2B = true
			replacement.ExpiresAt = now.Add(time.Hour).UnixNano()
			if _, err := store.Update(ctx, replacement); err != nil {
				t.Fatal(err)
			}
			if err := svc.ReconcileSandbox(ctx, replacement.ID, replacement.RuntimeID, now); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.GetSandbox(ctx, replacement.ID); err != nil {
				t.Fatal("maintenance ignored current deadline")
			}
			replacement.State = sandbox.StateUnknown
			if _, err := store.Update(ctx, replacement); err != nil {
				t.Fatal(err)
			}
			if err := svc.ReconcileSandbox(ctx, replacement.ID, replacement.RuntimeID, now); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.GetSandbox(ctx, replacement.ID); !errors.Is(err, sandbox.ErrNotFound) {
				t.Fatalf("eligible cleanup did not run: %v", err)
			}
			if err := svc.Capacity.reserve("extra", 2, 512); err != nil {
				t.Fatalf("eligible cleanup retained capacity: %v", err)
			}
		})
	}
}

func TestDelayedExitDoesNotRetireReusedSandboxID(t *testing.T) {
	ctx := context.Background()
	store := newMemorySandboxStore()
	templateID := digest.FromString("runtime-identity-template").String()
	ops := &fakeSandboxOps{store: store}
	svc := New(ops, nil)
	svc.Store = store
	setFakeTemplate(svc, testTemplateName, templateID, conchtemplate.BootModeCold)
	var err error
	svc.Capacity, err = NewCapacity(CapacityLimits{MaxSandboxes: 1, MaxCPUs: 2, MaxMemoryMB: 512})
	if err != nil {
		t.Fatal(err)
	}
	opts := SandboxCreateOptions{SandboxID: "reused-id", TemplateName: testTemplateName, VCPUNum: 2, VCPUMax: 2, RamMB: 512}
	if _, err := svc.CreateSandbox(ctx, opts); err != nil {
		t.Fatal(err)
	}
	old, err := svc.GetSandbox(ctx, opts.SandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveSandbox(ctx, opts.SandboxID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateSandbox(ctx, opts); err != nil {
		t.Fatal(err)
	}
	replacement, err := svc.GetSandbox(ctx, opts.SandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Capacity.reserve(replacement.ID, replacement.VCPUNum, replacement.RamMB); err != nil {
		t.Fatal(err)
	}
	if old.RuntimeID == "" || replacement.RuntimeID == "" || old.RuntimeID == replacement.RuntimeID || ops.req.RuntimeID != replacement.RuntimeID {
		t.Fatalf("runtime identity was reused: old=%+v new=%+v", old, replacement)
	}
	// This is the original observer arriving after DELETE + native ID reuse.
	// Manager has no record for the exited runtime, so the stale exit is a
	// no-op and must not touch the replacement or refund its reservation.
	svc.HandleSandboxUnexpectedExit(old.ID, old.RuntimeID, nil)
	after, err := svc.GetSandbox(ctx, replacement.ID)
	if err != nil || !reflect.DeepEqual(after, replacement) {
		t.Fatalf("stale exit changed replacement: %+v err=%v", after, err)
	}
	if err := svc.Capacity.reserve("extra", 2, 512); !errors.Is(err, sandbox.ErrResourceExhausted) {
		t.Fatalf("stale exit refunded replacement capacity: %v", err)
	}
	// The replacement's own exit releases its reservation through the record
	// Manager persisted for it. The record itself stays behind as Manager's
	// UNKNOWN owner; a fresh ID can consume the freed capacity.
	svc.HandleSandboxUnexpectedExit(replacement.ID, replacement.RuntimeID, nil)
	if _, err := svc.GetSandbox(ctx, replacement.ID); err != nil {
		t.Fatalf("unexpected exit removed a Manager-owned record: %v", err)
	}
	if err := svc.Capacity.reserve("extra", 2, 512); err != nil {
		t.Fatalf("current exit did not release capacity: %v", err)
	}
}
