package conchbuild

// Snapshot label keys aligned with Conch snapshotter.
// These labels associate rootfs, mem, and VM snapshots in containerd.
const (
	SnapshotLabelMemSnapshot    = "conch/snapshotter/mem-snapshot"
	SnapshotLabelRootfsSnapshot = "conch/snapshotter/rootfs-snapshot"
	SnapshotLabelVMSnapshot     = "conch/snapshotter/vm-snapshot"
)
