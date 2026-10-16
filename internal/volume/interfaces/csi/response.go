package csidriver

import (
	"github.com/container-storage-interface/spec/lib/go/csi"
	"pve-csi.local/csi-plugin/internal/volume/domain"
)

func createResponse(volume domain.Volume) *csi.CreateVolumeResponse {
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{
		VolumeId: volume.ID(), CapacityBytes: volume.CapacityBytes(),
		AccessibleTopology: []*csi.Topology{{Segments: map[string]string{pveNodeTopologyKey: volume.Placement().Node}}},
	}}
}
