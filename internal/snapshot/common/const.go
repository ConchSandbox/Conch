package common

const (
	DirMode  = 0750
	FileMode = 0640

	MemFileName          = "mem.img"
	MemKeySuffix         = "-mem"
	VmKeySuffix          = "-vm"
	MemMB                = (1024 * 1024)
	MemFileDefaultSize   = 256 // 256MB
	VmInitrdRelativePath = "data/conch.initrd"
	VmKernelRelativePath = "boot/vmlinuz"
	PmemPrefix           = "layer"
	PemSuffix            = ".erofs"

	SnapshotMountRootfs = "rootfs"
	SnapshotMountMem    = "mem"
	SnapshotMountVM     = "vm"

	ImageKindVirtualMachine = "virtual-machine"
	ImageKindMemSnapshot    = "snapshot"
	ImageKindRootfs         = "rootfs"

	SnapshotLabel            = "conch/snapshotter/snapshot"
	SnapshotLabelRootfs      = "conch/snapshotter/snapshot-rootfs"
	SnapshotLabelSnapshotDir = "conch/snapshotter/snapshot-dir"
	SnapshotLabelMemSize     = "conch/snapshotter/snapshot-memsize"

	// SnapshotLabelRootfsImage records the source image name used to create the
	// rootfs snapshot.
	SnapshotLabelRootfsImage = "conch/snapshotter/rootfs-image"

	// SnapshotLabelRootfsManifest records the source image manifest digest used
	// to create the rootfs snapshot.
	SnapshotLabelRootfsManifest = "conch/snapshotter/rootfs-manifest"
)
