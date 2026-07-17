package conchplugins

import "github.com/containerd/plugin"

const (
	HostPluginType plugin.Type = "io.conch.internal.v1"
	HostPluginID               = "containerd-host"
	HostPluginURI              = string(HostPluginType) + "." + HostPluginID

	ImageServicePluginType    plugin.Type = "io.conch.image.v1"
	ImageServiceID                        = "image"
	ImageServiceURI                       = string(ImageServicePluginType) + "." + ImageServiceID
	SnapshotServicePluginType plugin.Type = "io.conch.snapshot.v1"
	SnapshotServiceID                     = "snapshot"
	SnapshotServiceURI                    = string(SnapshotServicePluginType) + "." + SnapshotServiceID
	TemplateServicePluginType plugin.Type = "io.conch.template.v1"
	TemplateServiceID                     = "template"
	TemplateServiceURI                    = string(TemplateServicePluginType) + "." + TemplateServiceID
	SandboxServicePluginType  plugin.Type = "io.conch.sandbox.v1"
	SandboxServiceID                      = "sandbox"
	SandboxServiceURI                     = string(SandboxServicePluginType) + "." + SandboxServiceID
)
