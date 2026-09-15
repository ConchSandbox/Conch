// Package cluster implements Conch's AgentENV Scheduler integration.
package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	schedulerv1 "github.com/openeuler/Conch/internal/cluster/schedulerv1"
	"github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/internal/version"
)

// Config describes a single node's identity and its Scheduler connection.
// SchedulerAddr is a host:port or http://host:port endpoint. The cluster MVP
// uses the Scheduler's plaintext gRPC listener on the private node network.
type Config struct {
	SchedulerAddr string
	NodeID        string
	ClusterID     string
	Interval      time.Duration
	RPCTimeout    time.Duration
}

// RuntimeStatsSource exposes lifecycle counters and the runtime's membership
// gate. Counters are cumulative for this process, including deleted sandboxes.
type RuntimeStatsSource interface {
	CreateCounts() (successes, failures uint64)
	// LockSandboxRoster excludes insertion/deletion of persistent sandbox IDs
	// until the returned unlock function is called. Holding this through the
	// heartbeat RPC prevents an older full roster from deleting a binding for
	// a newly created sandbox after Gateway records its assignment.
	LockSandboxRoster() func()
}

// Reporter publishes complete store rosters, resource allocation, and host
// utilization. Its lifetime belongs to the daemon, independently of API calls.
type Reporter struct {
	config            Config
	store             sandbox.Store
	stats             RuntimeStatsSource
	host              *hostCollector
	serviceInstanceID string
	conn              *grpc.ClientConn
	client            schedulerv1.SchedulerClient

	mu          sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
	closed      bool
	shutdownErr error
}

// NewReporter prepares a reporter without starting background work. Start must
// be called only after stale-resource cleanup and the Node TCP listener are ready.
// A Ready heartbeat is an observation, not a Scheduler admission gate.
func NewReporter(config Config, store sandbox.Store, stats RuntimeStatsSource) (*Reporter, error) {
	config.NodeID = strings.TrimSpace(config.NodeID)
	config.ClusterID = strings.TrimSpace(config.ClusterID)
	if config.NodeID == "" || config.ClusterID == "" {
		return nil, fmt.Errorf("cluster node_id and cluster_id are required")
	}
	if config.Interval <= 0 || config.RPCTimeout <= 0 {
		return nil, fmt.Errorf("heartbeat interval and RPC timeout must be positive")
	}
	if store == nil || stats == nil {
		return nil, fmt.Errorf("heartbeat sandbox store and runtime statistics are required")
	}
	addr, err := schedulerAddress(config.SchedulerAddr)
	if err != nil {
		return nil, err
	}
	host, err := newHostCollector()
	if err != nil {
		return nil, fmt.Errorf("initialize host metrics: %w", err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("create scheduler client: %w", err)
	}
	return &Reporter{
		config: config, store: store, stats: stats, host: host,
		serviceInstanceID: uuid.NewString(), conn: conn,
		client: schedulerv1.NewSchedulerClient(conn),
	}, nil
}

func schedulerAddress(raw string) (string, error) {
	addr := strings.TrimSpace(raw)
	if strings.Contains(addr, "://") {
		u, err := url.Parse(addr)
		if err != nil || u.Scheme != "http" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return "", fmt.Errorf("invalid plaintext scheduler endpoint %q", raw)
		}
		addr = u.Host
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || strings.TrimSpace(host) == "" {
		return "", fmt.Errorf("scheduler endpoint must be host:port: %q", raw)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("invalid scheduler port in %q", raw)
	}
	return addr, nil
}

// Start sends the first heartbeat immediately, then reports at Config.Interval.
// Each collection and RPC has its own timeout. A failed collection is skipped
// instead of publishing an empty roster that would erase Scheduler bindings.
func (r *Reporter) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return fmt.Errorf("cluster reporter is closed")
	}
	if r.cancel != nil {
		return fmt.Errorf("cluster reporter already started")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, r.cancel = context.WithCancel(ctx)
	r.done = make(chan struct{})
	go r.run(ctx)
	return nil
}

func (r *Reporter) run(ctx context.Context) {
	defer close(r.done)
	ticker := time.NewTicker(r.config.Interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := r.heartbeat(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("Scheduler heartbeat failed", "node_id", r.config.NodeID, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Reporter) heartbeat(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.config.RPCTimeout)
	defer cancel()
	unlock := r.stats.LockSandboxRoster()
	defer unlock()
	records, err := r.store.List(ctx, sandbox.Filter{})
	if err != nil {
		return fmt.Errorf("collect sandbox roster: %w", err)
	}
	snapshot, ids, err := resourceSnapshot(records)
	if err != nil {
		return err
	}
	if err := r.host.collect(snapshot); err != nil {
		return fmt.Errorf("collect host metrics: %w", err)
	}
	snapshot.CreateSuccesses, snapshot.CreateFails = r.stats.CreateCounts()
	snapshot.ReportedAtUnixMs = time.Now().UnixMilli()
	_, err = r.client.Heartbeat(ctx, &schedulerv1.HeartbeatRequest{
		NodeId: r.config.NodeID, ClusterId: r.config.ClusterID,
		ServiceInstanceId: r.serviceInstanceID,
		Version:           version.Version, Commit: version.Commit,
		MachineInfo: r.host.machine, Snapshot: snapshot, SandboxIds: ids,
	})
	return err
}

// Shutdown stops reporting before unregistering, so a late heartbeat cannot
// recreate the node's bindings after graceful shutdown. Call with a fresh
// shutdown context, even when the daemon's main context has been canceled.
func (r *Reporter) Shutdown(ctx context.Context) (shutdownErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.shutdownErr
	}
	r.closed = true
	defer func() {
		r.shutdownErr = errors.Join(shutdownErr, r.conn.Close())
		shutdownErr = r.shutdownErr
	}()
	if r.cancel == nil {
		return nil
	}
	r.cancel()
	select {
	case <-r.done:
	case <-ctx.Done():
		r.shutdownErr = ctx.Err()
		return r.shutdownErr
	}
	ctx, cancel := context.WithTimeout(ctx, r.config.RPCTimeout)
	defer cancel()
	_, err := r.client.UnregisterNode(ctx, &schedulerv1.UnregisterNodeRequest{
		NodeId: r.config.NodeID, ServiceInstanceId: r.serviceInstanceID,
	})
	if err != nil {
		r.shutdownErr = fmt.Errorf("unregister scheduler node: %w", err)
	}
	return r.shutdownErr
}
