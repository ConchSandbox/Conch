package common

const (
	DirMode  = 0750
	FileMode = 0640

	MemFileName            = "mem.img"
	SnapshotConfigFileName = "config.json"
	MemKeySuffix           = "-mem"
	VmKeySuffix            = "-vm"
	MemMB                  = (1024 * 1024)
	MemFileDefaultSize     = 256 // 256MB
	VmInitrdRelativePath   = "data/conch.initrd"
	VmKernelRelativePath   = "boot/vmlinuz"
	PmemPrefix             = "layer"
	PemSuffix              = ".erofs"

	ContainerdSock = "/run/containerd/containerd.sock"

	SnapshotMountRootfs = "rootfs"
	SnapshotMountMem    = "mem"
	SnapshotMountVM     = "vm"
	SnapshotSharedDir   = "shared"

	ImageKindVirtualMachine = "virtual-machine"
	ImageKindMemSnapshot    = "snapshot"
	ImageKindRootfs         = "rootfs"

	SnapshotLabel            = "conch/snapshotter/snapshot"
	SnapshotLabelRootfs      = "conch/snapshotter/snapshot-rootfs"
	SnapshotLabelSnapshotDir = "conch/snapshotter/snapshot-dir"
	SnapshotLabelMemSize     = "conch/snapshotter/snapshot-memsize"

	// binding labels between mem and rootfs snapshots
	SnapshotLabelMemSnapshot    = "conch/snapshotter/mem-snapshot"
	SnapshotLabelRootfsSnapshot = "conch/snapshotter/rootfs-snapshot"
	SnapshotLabelVMSnapshot     = "conch/snapshotter/vm-snapshot"
)
