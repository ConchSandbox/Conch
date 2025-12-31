package common

const (
	DirMode  = 0750
	FileMode = 0640

	MemFileName        = "memfile.img"
	MEMMB              = (1024 * 1024)
	MemFileDefaultSize = (256 * MEMMB) // 256MB

	CONTAINERD_SOCK = "/run/containerd/containerd.sock"
)
