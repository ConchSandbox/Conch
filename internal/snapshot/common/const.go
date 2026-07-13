package common

const (
	DirMode  = 0750
	FileMode = 0640

	MemFileName                   = "mem.img"
	SnapshotConfigFileName        = "config.json"
	SnapshotConfigPmemDeviceCount = "pmem_device_count"
	MemKeySuffix                  = "-mem"
	VmKeySuffix                   = "-vm"
	MemMB                         = (1024 * 1024)
	MemFileDefaultSize            = 256 // 256MB
	VmInitrdRelativePath          = "data/conch.initrd"
	VmKernelRelativePath          = "boot/vmlinuz"
	PmemPrefix                    = "layer"
	PemSuffix                     = ".erofs"

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

	// Conch snapshot group labels. A group is anchored by its rootfs snapshot.

	// SnapshotLabelGroupID is set on a component snapshot and stores the rootfs
	// snapshot key of the group it belongs to.
	SnapshotLabelGroupID = "conch/snapshotter/group-id"

	// SnapshotLabelGroupMemRef is set on a group rootfs snapshot and points to
	// the group's mem snapshot component.
	SnapshotLabelGroupMemRef = "conch/snapshotter/group.mem-ref"

	// SnapshotLabelGroupVMRef is set on a group rootfs snapshot and points to
	// the group's VM/sandbox snapshot component.
	SnapshotLabelGroupVMRef = "conch/snapshotter/group.vm-ref"

	// SnapshotLabelComponentKind is set on a component snapshot to distinguish
	// mem and VM components within the group.
	SnapshotLabelComponentKind = "conch/snapshotter/component-kind"

	// SnapshotLabelRootfsImage records the source image name used to create the
	// rootfs snapshot.
	SnapshotLabelRootfsImage = "conch/snapshotter/rootfs-image"

	// SnapshotLabelRootfsManifest records the source image manifest digest used
	// to create the rootfs snapshot.
	SnapshotLabelRootfsManifest = "conch/snapshotter/rootfs-manifest"

	SnapshotComponentKindMem = "mem"
	SnapshotComponentKindVM  = "vm"
)
