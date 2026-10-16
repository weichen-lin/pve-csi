package csiserver

import (
	"context"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const DriverName = "pve-csi.local"

type Identity struct {
	csi.UnimplementedIdentityServer
	version string
}

func NewIdentity(version string) *Identity { return &Identity{version: version} }

func (s *Identity) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: DriverName, VendorVersion: s.version}, nil
}

func (s *Identity) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{Capabilities: []*csi.PluginCapability{
		{Type: &csi.PluginCapability_Service_{Service: &csi.PluginCapability_Service{Type: csi.PluginCapability_Service_CONTROLLER_SERVICE}}},
		{Type: &csi.PluginCapability_Service_{Service: &csi.PluginCapability_Service{Type: csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS}}},
	}}, nil
}

func (s *Identity) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	// Registration follows configuration validation. This reports initialization
	// only: it does not claim PVE connectivity or provisioner readiness.
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
