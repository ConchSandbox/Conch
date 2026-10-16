package cluster

import (
	"fmt"
	"math"
	"runtime"
	"sort"

	"github.com/prometheus/procfs"

	schedulerv1 "github.com/openeuler/Conch/internal/cluster/schedulerv1"
	"github.com/openeuler/Conch/internal/sandbox"
)

// resourceSnapshot projects one complete store read into both the allocation
// snapshot and the binding roster. Creating records reserve resources already.
// Conch's private SUSPENDED state retains its VMM and memory; it is still an
// active allocation, never AgentENV's resource-releasing paused state. Full E2B
// pause/resume and the corresponding paused_* metrics are future work.
func resourceSnapshot(records []sandbox.Record) (*schedulerv1.NodeSnapshot, []string, error) {
	snapshot := &schedulerv1.NodeSnapshot{Status: schedulerv1.NodeStatus_NODE_STATUS_READY}
	ids := make([]string, 0, len(records))
	for _, record := range records {
		if record.ID == "" || record.VCPUNum < 0 || record.RamMB < 0 {
			return nil, nil, fmt.Errorf("invalid heartbeat resource record %q", record.ID)
		}
		ids = append(ids, record.ID)
		// This fork has no released-but-retained state: Manager retains a
		// record exactly while its capacity reservation is unconfirmed, so
		// every persisted record still owns its CPU/memory allocation.
		if uint64(record.VCPUNum) > math.MaxUint32-uint64(snapshot.AllocatedCpu) ||
			uint64(record.RamMB) > (math.MaxUint64-snapshot.AllocatedMemoryBytes)/(1024*1024) {
			return nil, nil, fmt.Errorf("heartbeat resource allocation overflow at %q", record.ID)
		}
		snapshot.AllocatedCpu += uint32(record.VCPUNum)
		snapshot.AllocatedMemoryBytes += uint64(record.RamMB) * 1024 * 1024
		switch record.State {
		case sandbox.StateCreating:
			snapshot.SandboxStartingCount++
		case sandbox.StateReady, sandbox.StateSuspended:
			snapshot.SandboxCount++
		case sandbox.StateUnknown:
			// Cleanup has not released this failed runtime's resources yet.
		default:
			return nil, nil, fmt.Errorf("invalid heartbeat sandbox state %q", record.State)
		}
	}
	sort.Strings(ids)
	return snapshot, ids, nil
}

type hostCollector struct {
	fs       procfs.FS
	machine  *schedulerv1.MachineInfo
	previous procfs.CPUStat
}

func newHostCollector() (*hostCollector, error) {
	fs, err := procfs.NewDefaultFS()
	if err != nil {
		return nil, err
	}
	stat, err := fs.Stat()
	if err != nil {
		return nil, err
	}
	machine := &schedulerv1.MachineInfo{CpuArchitecture: runtime.GOARCH}
	info, err := fs.CPUInfo()
	if err == nil && len(info) > 0 {
		machine.CpuFamily = info[0].CPUFamily
		machine.CpuModel = info[0].Model
		machine.CpuModelName = info[0].ModelName
	}
	return &hostCollector{fs: fs, machine: machine, previous: stat.CPUTotal}, nil
}

func (h *hostCollector) collect(snapshot *schedulerv1.NodeSnapshot) error {
	stat, err := h.fs.Stat()
	if err != nil {
		return err
	}
	mem, err := h.fs.Meminfo()
	if err != nil {
		return err
	}
	if mem.MemTotalBytes == nil || mem.MemAvailableBytes == nil || *mem.MemAvailableBytes > *mem.MemTotalBytes {
		return fmt.Errorf("missing or invalid MemTotal/MemAvailable in /proc/meminfo")
	}
	snapshot.CpuCount = uint32(len(stat.CPU))
	snapshot.CpuPercent = cpuPercent(h.previous, stat.CPUTotal)
	snapshot.MemoryTotalBytes = *mem.MemTotalBytes
	snapshot.MemoryUsedBytes = *mem.MemTotalBytes - *mem.MemAvailableBytes
	h.previous = stat.CPUTotal
	return nil
}

func cpuPercent(previous, current procfs.CPUStat) uint32 {
	// Guest/GuestNice are included in User/Nice by Linux; summing them again
	// would double-count VM work. I/O wait is idle time for CPU utilization.
	total := func(s procfs.CPUStat) float64 {
		return s.User + s.Nice + s.System + s.Idle + s.Iowait + s.IRQ + s.SoftIRQ + s.Steal
	}
	delta := total(current) - total(previous)
	if delta <= 0 {
		return 0
	}
	idle := (current.Idle + current.Iowait) - (previous.Idle + previous.Iowait)
	return uint32(math.Round(max(0, min(100, 100*(delta-idle)/delta))))
}
