package ocipublisher

import (
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	VMDataLayerMediaType = v1.MediaTypeImageLayerGzip

	VMStateTypeFull           = "full-state"
	AnnotationVMStateType    = "io.conch.image.vm.state.type"
	AnnotationVMParentDigest = "io.conch.image.vm.parent.digest"
	VMKernelManifestMediaType = "application/vnd.conch.image.vm.kernel.manifest.v1+json"
	MediaTypeRootfsManifest   = v1.MediaTypeImageManifest
)

type PublishOptions struct {
	Tag            string
	ConfigFilePath string
	MemRangePath   string
	StateJSONPath  string
	RootfsImageID  string
	KernelImageID  string
}

type ConchManifest struct {
	v1.Manifest

	Annotations map[string]string `json:"annotations,omitempty"`
}
