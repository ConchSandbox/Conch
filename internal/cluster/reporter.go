// Package cluster implements Conch's AgentENV Scheduler integration.
package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
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
// SchedulerAddr is a host:port endpoint; config load normalizes any
// http://host:port form to it. The cluster MVP uses the Scheduler's
// plaintext gRPC listener on the private node network.
type Config struct {
	SchedulerAddr string
	NodeID        string
	ClusterID     string
	Interval      time.Duration
	RPCTimeout    time.Duration
}

// RuntimeStatsSource exposes lifecycle counters. Counters are cumulative for
// this process, including deleted sandboxes.
type RuntimeStatsSource interface {
	CreateCounts() (successes, failures uint64)
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

// Start sends the first heartbeat immediately, then waits Config.Interval
// after each heartbeat's end. One RPCTimeout covers the roster List, host
// collection and RPC together. A failed collection is skipped instead of
// publishing an empty roster that would erase Scheduler bindings.
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
	for {
		if ctx.Err() != nil {
			return
		}
		if err := r.heartbeat(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("Scheduler heartbeat failed", "node_id", r.config.NodeID, "error", err)
		}
		// The interval counts from each heartbeat's end, so a slow RPC delays
		// the next report instead of stacking on a fixed cadence.
		timer := time.NewTimer(r.config.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (r *Reporter) heartbeat(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.config.RPCTimeout)
	defer cancel()
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
