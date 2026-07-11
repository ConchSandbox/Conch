package image

import "errors"

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
	RootfsKey       string `json:"rootfs_key"`
	MemKey          string `json:"mem_key,omitempty"`
	VMKey           string `json:"vm_key"`
	ImageName       string `json:"image_name"`
}
