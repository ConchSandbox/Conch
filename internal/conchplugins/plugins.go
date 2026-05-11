package conchplugins

import "github.com/containerd/plugin"

const (
	HostPluginType plugin.Type = "io.conch.internal.v1"
	HostPluginID               = "containerd-host"
	HostPluginURI              = string(HostPluginType) + "." + HostPluginID

	ServicePluginType plugin.Type = "io.conch.service.v1"
	ImageServiceID                = "image"
	ImageServiceURI               = string(ServicePluginType) + "." + ImageServiceID
)
