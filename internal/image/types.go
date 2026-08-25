package image

import ocispec "github.com/opencontainers/image-spec/specs-go/v1"

const TemplateLabelRegistryPlainHTTP = "io.conch.internal.registry.plain-http"

// RegistryPullOptions contains the registry inputs shared by OCI image and
// Boot Index pull workflows. Command-specific kind policy is applied by the
// caller before child manifests and layers are fetched.
type RegistryPullOptions struct {
	Reference  string
	PlainHTTP  bool
	Username   string
	Password   string
	PreferLazy bool
}

type PublishBootIndexOptions struct {
	RootfsImageName string `json:"rootfs_image_name"`
	KernelPath      string `json:"kernel_path"`
	InitrdPath      string `json:"initrd_path"`
}

type PublishBootIndexResult struct {
	BootIndexDigest string `json:"boot_index_digest"`
	BuildRef        string `json:"build_ref"`
}

type PullBootIndexResult struct {
	Info     BootIndexInfo
	BuildRef string
}

// BootIndexInfo is the validated, content-addressed view of a Conch Boot
// Index. MemDescriptor is empty for a cold-boot index. VMMName is present for
// resume indexes and records the VMM that produced the captured state.
type BootIndexInfo struct {
	BootIndexDigest   string             `json:"boot_index_digest"`
	RootfsDescriptor  ocispec.Descriptor `json:"rootfs_descriptor"`
	MemDescriptor     ocispec.Descriptor `json:"mem_descriptor,omitempty"`
	SandboxDescriptor ocispec.Descriptor `json:"sandbox_descriptor"`
	Resume            bool               `json:"resume"`
	VMMName           string             `json:"vmm_name,omitempty"`
	MemorySizeMB      int64              `json:"memory_size_mb,omitempty"`
	PreGateProfile    string             `json:"pre_gate_profile,omitempty"`
}

// PublishCheckpointBootIndexOptions publishes captured memory and VMM state as
// a new Boot Index while reusing the source Boot Index's immutable rootfs and
// sandbox components. MemRoot is a self-contained directory whose artifact
// layout is defined by VMMName.
type PublishCheckpointBootIndexOptions struct {
	SourceBootIndexDigest string `json:"source_boot_index_digest"`
	MemRoot               string `json:"mem_root"`
	VMMName               string `json:"vmm_name"`
	MemorySizeMB          int64  `json:"memory_size_mb"`
	// AnnotateMemExtent enables dump.erofs inspection of the checkpoint memory
	// EROFS extent so the mem layer carries the file offset/size annotations
	// consumed by pre-gate lazy pull. It mirrors the pre-gate feature toggle;
	// the main publish path leaves it false and never invokes dump.erofs.
	AnnotateMemExtent bool `json:"annotate_mem_extent,omitempty"`
}

// PublishCheckpointBootIndexResult deliberately contains no snapshot keys:
// publishing checkpoint content must not create checkpoint snapshots.
type PublishCheckpointBootIndexResult struct {
	BootIndexDigest string `json:"boot_index_digest"`
	BuildRef        string `json:"build_ref"`
}

// PushBootIndexOptions publishes the descriptor closure rooted at an
// immutable Boot Index digest. RemoteReference is only the registry name
// assigned at the destination and never participates in content identity.
type PushBootIndexOptions struct {
	BootIndexDigest string `json:"boot_index_digest"`
	RemoteReference string `json:"remote_reference"`
	PlainHTTP       bool   `json:"plain_http,omitempty"`
	Username        string `json:"username,omitempty"`
	Password        string `json:"password,omitempty"`
	PreGateProfile  []byte `json:"-"`
}
