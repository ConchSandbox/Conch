package containerdhost

import (
	_ "github.com/containerd/containerd/v2/core/runtime/v2"
	_ "github.com/containerd/containerd/v2/plugins/content/local/plugin"
	_ "github.com/containerd/containerd/v2/plugins/diff/walking/plugin"
	_ "github.com/containerd/containerd/v2/plugins/events"
	_ "github.com/containerd/containerd/v2/plugins/gc"
	_ "github.com/containerd/containerd/v2/plugins/imageverifier"
	_ "github.com/containerd/containerd/v2/plugins/leases"
	_ "github.com/containerd/containerd/v2/plugins/metadata"
	_ "github.com/containerd/containerd/v2/plugins/mount"
	_ "github.com/containerd/containerd/v2/plugins/services/containers"
	_ "github.com/containerd/containerd/v2/plugins/services/content"
	_ "github.com/containerd/containerd/v2/plugins/services/diff"
	_ "github.com/containerd/containerd/v2/plugins/services/images"
	_ "github.com/containerd/containerd/v2/plugins/services/introspection"
	_ "github.com/containerd/containerd/v2/plugins/services/namespaces"
	_ "github.com/containerd/containerd/v2/plugins/services/sandbox"
	_ "github.com/containerd/containerd/v2/plugins/services/snapshots"
	_ "github.com/containerd/containerd/v2/plugins/services/tasks"
	_ "github.com/containerd/containerd/v2/plugins/services/warning"
	_ "github.com/containerd/containerd/v2/plugins/snapshots/overlay/plugin"
	_ "github.com/containerd/containerd/v2/plugins/transfer"

	_ "github.com/openeuler/Conch/internal/conchservices/image"
)
