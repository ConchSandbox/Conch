package common

const (
	DirMode  = 0750
	FileMode = 0640

	MemFileName          = "mem.img"
	VMMStateDir          = "vmm-state"
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

	SnapshotLabel        = "conch/snapshotter/snapshot"
	SnapshotLabelRootfs  = "conch/snapshotter/snapshot-rootfs"
	SnapshotLabelMemSize = "conch/snapshotter/snapshot-memsize"
)
