package common

const (
	DirMode  = 0750
	FileMode = 0640

	MemFileName        = "mem.img"
	MemKeySuffix       = "-mem"
	TempViewPrefix     = "temp-view-"
	MemMB              = (1024 * 1024)
	MemFileDefaultSize = 256 // 256MB

	ContainerdSock = "/run/containerd/containerd.sock"
)
