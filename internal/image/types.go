package image

import (
	"errors"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

var (
	ErrInvalidRequest      = errors.New("invalid image request")
	ErrOCIConversionFailed = errors.New("oci conversion failed")
)

type PrepareRootfsSourceOptions struct {
	Source      string `json:"source"`
	Namespace   string `json:"namespace,omitempty"`
	TargetImage string `json:"target_image,omitempty"`
	PlainHTTP   bool   `json:"plain_http,omitempty"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
}

type PrepareRootfsSourceResult struct {
	ImageName      string `json:"image_name"`
	ManifestDigest string `json:"manifest_digest"`
}

type PublishBootImageOptions struct {
	Namespace       string `json:"namespace,omitempty"`
	RootfsImageName string `json:"rootfs_image_name"`
	KernelPath      string `json:"kernel_path"`
	InitrdPath      string `json:"initrd_path"`
	BootIndexTag    string `json:"boot_index_tag"`
}

type PublishBootImageResult struct {
	BootIndexDigest string `json:"boot_index_digest"`
	ImageName       string `json:"image_name"`
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
}

// PublishCheckpointBootImageOptions publishes captured memory and VMM state as
// a new Boot Index while reusing the source Boot Index's immutable rootfs and
// sandbox components. MemRoot is a self-contained directory whose artifact
// layout is defined by VMMName.
type PublishCheckpointBootImageOptions struct {
	Namespace             string `json:"namespace,omitempty"`
	SourceBootIndexDigest string `json:"source_boot_index_digest"`
	BootIndexTag          string `json:"boot_index_tag"`
	MemRoot               string `json:"mem_root"`
	VMMName               string `json:"vmm_name"`
	MemorySizeMB          int64  `json:"memory_size_mb"`
}

// PublishCheckpointBootImageResult deliberately contains no snapshot keys:
// publishing checkpoint content must not create checkpoint snapshots.
type PublishCheckpointBootImageResult struct {
	BootIndexDigest string `json:"boot_index_digest"`
	ImageName       string `json:"image_name"`
}

// PushBootIndexOptions publishes the descriptor closure rooted at an
// immutable Boot Index digest. RemoteReference is only the registry name
// assigned at the destination and never participates in content identity.
type PushBootIndexOptions struct {
	Namespace       string `json:"namespace,omitempty"`
	BootIndexDigest string `json:"boot_index_digest"`
	RemoteReference string `json:"remote_reference"`
	PlainHTTP       bool   `json:"plain_http,omitempty"`
	Username        string `json:"username,omitempty"`
	Password        string `json:"password,omitempty"`
	RegistryTimeout string `json:"registry_timeout,omitempty"`
}
