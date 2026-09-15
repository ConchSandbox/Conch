package cluster

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	containerdsandbox "github.com/openeuler/Conch/internal/adapters/containerd/sandbox"
	schedulerv1 "github.com/openeuler/Conch/internal/cluster/schedulerv1"
	"github.com/openeuler/Conch/internal/conchruntime"
	"github.com/openeuler/Conch/internal/sandbox"
)

type recordingScheduler struct {
	schedulerv1.UnimplementedSchedulerServer
	heartbeats   chan *schedulerv1.HeartbeatRequest
	unregisters  chan *schedulerv1.UnregisterNodeRequest
	block        bool
	rejectFirst  atomic.Bool
	canceled     chan struct{}
	responseGate chan struct{}
}

func (s *recordingScheduler) Heartbeat(ctx context.Context, req *schedulerv1.HeartbeatRequest) (*schedulerv1.HeartbeatResponse, error) {
	s.heartbeats <- req
	if s.block {
		<-ctx.Done()
		close(s.canceled)
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	if s.rejectFirst.CompareAndSwap(true, false) {
		return nil, status.Error(codes.Unavailable, "scheduler temporarily unavailable")
	}
	if s.responseGate != nil {
		select {
		case <-s.responseGate:
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	return &schedulerv1.HeartbeatResponse{}, nil
}

func (s *recordingScheduler) UnregisterNode(_ context.Context, req *schedulerv1.UnregisterNodeRequest) (*schedulerv1.UnregisterNodeResponse, error) {
	s.unregisters <- req
	return &schedulerv1.UnregisterNodeResponse{}, nil
}

type createStats struct {
	conchruntime.Service
	successes, failures uint64
}

func (s *createStats) CreateCounts() (uint64, uint64) { return s.successes, s.failures }

func startScheduler(t *testing.T, server *recordingScheduler) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	schedulerv1.RegisterSchedulerServer(s, server)
	go func() { _ = s.Serve(listener) }()
	t.Cleanup(s.Stop)
	return listener.Addr().String()
}

func newRecordingScheduler() *recordingScheduler {
	return &recordingScheduler{
		heartbeats:  make(chan *schedulerv1.HeartbeatRequest, 32),
		unregisters: make(chan *schedulerv1.UnregisterNodeRequest, 4),
		canceled:    make(chan struct{}),
	}
}

func newPersistentStore(t *testing.T) sandbox.Store {
	t.Helper()
	dir := t.TempDir()
	bdb, err := bolt.Open(filepath.Join(dir, "metadata.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bdb.Close() })
	content, err := local.NewStore(filepath.Join(dir, "content"))
	if err != nil {
		t.Fatal(err)
	}
	db := metadata.NewDB(bdb, content, nil)
	if err := db.Init(containerdclient.NewNamespaceContext(context.Background())); err != nil {
		t.Fatal(err)
	}
	return containerdsandbox.NewStore(metadata.NewSandboxStore(db))
}

func putRecord(t *testing.T, store sandbox.Store, id string, state sandbox.State, cpu, mem int64) {
	t.Helper()
	_, err := store.Create(context.Background(), sandbox.Record{
		ID: id, State: state, VCPUNum: cpu, RamMB: mem,
		SourceTemplateID: "sha256:source", CheckpointHeadTemplateID: "sha256:head",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func receiveHeartbeat(t *testing.T, s *recordingScheduler) *schedulerv1.HeartbeatRequest {
	t.Helper()
	select {
	case request := <-s.heartbeats:
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("no heartbeat received")
		return nil
	}
}

func shutdownReporter(t *testing.T, reporter *Reporter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := reporter.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestReporterImmediatelyPublishesPersistentRosterAndUnregisters(t *testing.T) {
	server := newRecordingScheduler()
	addr := startScheduler(t, server)
	store := newPersistentStore(t)
	putRecord(t, store, "running", sandbox.StateReady, 2, 512)
	putRecord(t, store, "creating", sandbox.StateCreating, 4, 1024)
	putRecord(t, store, "suspended", sandbox.StateSuspended, 1, 256)
	putRecord(t, store, "unknown", sandbox.StateUnknown, 1, 128)
	reporter, err := NewReporter(Config{
		SchedulerAddr: "http://" + addr, NodeID: "node-a", ClusterID: "cluster-a",
		Interval: time.Hour, RPCTimeout: 3 * time.Second,
	}, store, &createStats{successes: 9, failures: 3})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shutdownReporter(t, reporter) })
	if err := reporter.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	req := receiveHeartbeat(t, server)
	if req.NodeId != "node-a" || req.ClusterId != "cluster-a" {
		t.Fatalf("incorrect node identity: %v", req)
	}
	if _, err := uuid.Parse(req.ServiceInstanceId); err != nil {
		t.Fatalf("service instance ID is not a UUID: %q", req.ServiceInstanceId)
	}
	if want := []string{"creating", "running", "suspended", "unknown"}; !reflect.DeepEqual(req.SandboxIds, want) {
		t.Fatalf("roster = %v, want %v", req.SandboxIds, want)
	}
	snapshot := req.Snapshot
	if snapshot.Status != schedulerv1.NodeStatus_NODE_STATUS_READY || snapshot.SandboxCount != 2 || snapshot.SandboxStartingCount != 1 || snapshot.PausedSandboxCount != 0 {
		t.Fatalf("incorrect lifecycle projection: %v", snapshot)
	}
	// Every persisted record still owns its allocation in this fork: Manager
	// retains a record exactly until cleanup confirms resource release.
	if snapshot.AllocatedCpu != 8 || snapshot.AllocatedMemoryBytes != 1920*1024*1024 || snapshot.PausedAllocatedCpu != 0 || snapshot.PausedAllocatedMemoryBytes != 0 {
		t.Fatalf("incorrect resource allocation: %v", snapshot)
	}
	if snapshot.CreateSuccesses != 9 || snapshot.CreateFails != 3 {
		t.Fatalf("incorrect cumulative counters: %v", snapshot)
	}
	if snapshot.CpuCount == 0 || snapshot.CpuPercent > 100 || snapshot.MemoryTotalBytes == 0 || snapshot.MemoryUsedBytes > snapshot.MemoryTotalBytes || snapshot.ReportedAtUnixMs <= 0 {
		t.Fatalf("invalid host metrics: %v", snapshot)
	}
	if req.MachineInfo == nil || req.MachineInfo.CpuArchitecture == "" {
		t.Fatal("missing machine info")
	}
	shutdownReporter(t, reporter)
	select {
	case unregister := <-server.unregisters:
		if unregister.NodeId != req.NodeId || unregister.ServiceInstanceId != req.ServiceInstanceId {
			t.Fatalf("unregister identity differs from heartbeat: %v", unregister)
		}
	default:
		t.Fatal("shutdown did not unregister node")
	}
	if err := reporter.Start(context.Background()); err == nil {
		t.Fatal("closed reporter allowed restart")
	}
}

func TestReporterRetriesAndRefreshesCompleteRoster(t *testing.T) {
	server := newRecordingScheduler()
	server.rejectFirst.Store(true)
	addr := startScheduler(t, server)
	store := newPersistentStore(t)
	putRecord(t, store, "old", sandbox.StateReady, 2, 512)
	reporter, err := NewReporter(Config{
		SchedulerAddr: addr, NodeID: "node-a", ClusterID: "cluster-a",
		Interval: 20 * time.Millisecond, RPCTimeout: time.Second,
	}, store, &createStats{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shutdownReporter(t, reporter) })
	if err := reporter.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := receiveHeartbeat(t, server)
	if err := store.Delete(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}
	putRecord(t, store, "new", sandbox.StateCreating, 4, 1024)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case next := <-server.heartbeats:
			if !reflect.DeepEqual(next.SandboxIds, []string{"new"}) {
				continue
			}
			if next.ServiceInstanceId != first.ServiceInstanceId || next.Snapshot.AllocatedCpu != 4 || next.Snapshot.AllocatedMemoryBytes != 1024*1024*1024 || next.Snapshot.SandboxStartingCount != 1 || next.Snapshot.SandboxCount != 0 {
				t.Fatalf("incorrect updated heartbeat: %v", next)
			}
			return
		case <-deadline:
			t.Fatal("reporter did not retry with the updated complete roster")
		}
	}
}

func TestReporterShutdownCancelsInflightHeartbeat(t *testing.T) {
	server := newRecordingScheduler()
	server.block = true
	addr := startScheduler(t, server)
	reporter, err := NewReporter(Config{
		SchedulerAddr: addr, NodeID: "node-a", ClusterID: "cluster-a",
		Interval: time.Hour, RPCTimeout: time.Hour,
	}, newPersistentStore(t), &createStats{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shutdownReporter(t, reporter) })
	if err := reporter.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	receiveHeartbeat(t, server)
	shutdownReporter(t, reporter)
	select {
	case <-server.canceled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel the in-flight heartbeat")
	}
	if len(server.unregisters) != 1 {
		t.Fatalf("unregister count = %d, want 1", len(server.unregisters))
	}
}

func TestReporterFreshIdentityAndCanceledStart(t *testing.T) {
	store := newPersistentStore(t)
	cfg := Config{SchedulerAddr: "127.0.0.1:9090", NodeID: "n", ClusterID: "c", Interval: time.Second, RPCTimeout: time.Second}
	a, err := NewReporter(cfg, store, &createStats{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shutdownReporter(t, a) })
	b, err := NewReporter(cfg, store, &createStats{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shutdownReporter(t, b) })
	if a.serviceInstanceID == b.serviceInstanceID {
		t.Fatal("reporters reused a service instance ID")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start(canceled) error = %v", err)
	}
}

func TestReporterSerializesInFlightRosterWithStoreInsertion(t *testing.T) {
	server := newRecordingScheduler()
	server.responseGate = make(chan struct{})
	addr := startScheduler(t, server)
	store := newPersistentStore(t)
	runtime := &conchruntime.Service{}
	reporter, err := NewReporter(Config{
		SchedulerAddr: addr, NodeID: "node-a", ClusterID: "cluster-a",
		Interval: time.Hour, RPCTimeout: 5 * time.Second,
	}, store, runtime)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shutdownReporter(t, reporter) })
	if err := reporter.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := receiveHeartbeat(t, server)
	if len(first.SandboxIds) != 0 {
		t.Fatalf("initial roster = %v, want empty", first.SandboxIds)
	}

	// Use the actual runtime membership gate around an actual persistent store
	// insertion, as CreateSandbox does, while the peer has not acknowledged the
	// preceding empty roster. No test-only production callback is involved.
	started := make(chan struct{})
	created := make(chan error, 1)
	go func() {
		close(started)
		unlock := runtime.LockSandboxRoster()
		defer unlock()
		_, err := store.Create(context.Background(), sandbox.Record{
			ID: "new", State: sandbox.StateCreating,
			SourceTemplateID: "sha256:source", VCPUNum: 2, RamMB: 512,
		})
		created <- err
	}()
	<-started
	select {
	case err := <-created:
		t.Fatalf("store insertion passed an unacknowledged heartbeat: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(server.responseGate)
	select {
	case err := <-created:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("store insertion did not proceed after heartbeat acknowledgment")
	}
	if _, err := store.Get(context.Background(), "new"); err != nil {
		t.Fatalf("new sandbox record is missing: %v", err)
	}
}
